// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/moby/sys/capability"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/control/server"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/eventfd"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/pipefs"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/signalfd"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/timerfd"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/kernel/shm"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/seccheck"
	"gvisor.dev/gvisor/pkg/sentry/socket"
	sentrynetstack "gvisor.dev/gvisor/pkg/sentry/socket/netstack"
	unixsocket "gvisor.dev/gvisor/pkg/sentry/socket/unix"
	unixtransport "gvisor.dev/gvisor/pkg/sentry/socket/unix/transport"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/fsgofer"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
)

func init() {
	// Reserve the initial thread for main so capability tests use threads that
	// can exit if restoration fails. Locking during init pins main to this thread:
	// https://pkg.go.dev/runtime#LockOSThread.
	runtime.LockOSThread()

	log.SetLevel(log.Debug)
	if err := fsgofer.OpenProcSelfFD("/proc/self/fd"); err != nil {
		panic(err)
	}
}

func testConfig() *config.Config {
	testFlags := flag.NewFlagSet("test", flag.ContinueOnError)
	config.RegisterFlags(testFlags)
	if err := configureHostTestFlags(testFlags); err != nil {
		panic(err)
	}
	conf, err := config.NewFromFlags(testFlags)
	if err != nil {
		panic(err)
	}
	// Change test defaults.
	conf.RootDir = "unused_root_dir"
	conf.Network = config.NetworkNone
	conf.DisableSeccomp = true
	return conf
}

// testSpec returns a simple spec that can be used in tests.
func testSpec() *specs.Spec {
	return &specs.Spec{
		// The host filesystem root is the sandbox root.
		Root: &specs.Root{
			Path:     "/",
			Readonly: true,
		},
		Process: &specs.Process{
			Args: []string{"/bin/true"},
		},
	}
}

// startGofer starts a new gofer routine serving 'root' path. It returns the
// sandbox side of the connection, and a function that when called will stop the
// gofer.
func startGofer(root string, conf *config.Config) (int, func(), error) {
	sandboxSocket, socket, err := unet.SocketPair(false)
	if err != nil {
		return 0, nil, err
	}
	sandboxEnd, err := sandboxSocket.Release()
	if err != nil {
		sandboxSocket.Close()
		socket.Close()
		return 0, nil, fmt.Errorf("releasing sandbox socket: %v", err)
	}
	server := lisafs.NewServer()
	connImpl := fsgofer.NewConnectionImpl(&fsgofer.Config{
		HostUDS:            conf.GetHostUDS(),
		HostFifo:           conf.HostFifo,
		DonateMountPointFD: conf.DirectFS,
	})
	c, err := server.CreateConnection(socket, root, fsgofer.ConnectionOpts(true), connImpl)
	if err != nil {
		return 0, nil, err
	}
	server.StartConnection(c)
	// Closing the gofer socket will stop the gofer and exit goroutine above.
	cleanup := func() {
		if err := socket.Close(); err != nil {
			log.Warningf("Error closing gofer socket: %v", err)
		}
		server.Wait()
		server.Destroy()
	}
	return sandboxEnd, cleanup, nil
}

func createLoader(conf *config.Config, spec *specs.Spec) (*Loader, func(), error) {
	return createLoaderWithGoferMountConf(conf, spec, specutils.GoferMountConf{Lower: specutils.Lisafs, Upper: specutils.NoOverlay})
}

func createLoaderWithGoferMountConf(conf *config.Config, spec *specs.Spec, mountConf specutils.GoferMountConf) (*Loader, func(), error) {
	return createLoaderWithGoferMountConfs(conf, spec, []specutils.GoferMountConf{mountConf})
}

func createLoaderWithGoferMountConfs(conf *config.Config, spec *specs.Spec, mountConfs []specutils.GoferMountConf) (*Loader, func(), error) {
	sock := fmt.Sprintf("\x00loader-test.%010d", rand.Int())
	controllerFD, err := server.CreateSocket(sock)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create socket: %w", err)
	}
	goferRoots := []string{spec.Root.Path}
	for _, mount := range spec.Mounts {
		if specutils.HasMountConfig(mount) {
			goferRoots = append(goferRoots, mount.Source)
		}
	}
	if len(goferRoots) != len(mountConfs) {
		unix.Close(controllerFD)
		return nil, nil, fmt.Errorf("got %d gofer mount configurations for %d roots", len(mountConfs), len(goferRoots))
	}
	goferEnds := make([]int, 0, len(goferRoots))
	cleanups := make([]func(), 0, len(goferRoots))
	cleanupGofers := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	for _, root := range goferRoots {
		sandEnd, cleanup, err := startGofer(root, conf)
		if err != nil {
			for _, goferFD := range goferEnds {
				unix.Close(goferFD)
			}
			cleanupGofers()
			unix.Close(controllerFD)
			return nil, nil, fmt.Errorf("failed to start gofer for %q: %w", root, err)
		}
		goferEnds = append(goferEnds, sandEnd)
		cleanups = append(cleanups, cleanup)
	}

	// Loader takes ownership of stdio.
	var stdio []int
	for _, f := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		hostFD := int(f.Fd())
		newFD, err := unix.Dup(hostFD)
		if err != nil {
			for _, goferFD := range goferEnds {
				unix.Close(goferFD)
			}
			for _, stdioFD := range stdio {
				unix.Close(stdioFD)
			}
			cleanupGofers()
			unix.Close(controllerFD)
			return nil, nil, fmt.Errorf("failed to dup FD %d: %w", hostFD, err)
		}
		stdio = append(stdio, newFD)
	}

	args := Args{
		ID:               "foo",
		Spec:             spec,
		Conf:             conf,
		ControllerFD:     controllerFD,
		GoferFDs:         goferEnds,
		DevGoferFD:       -1,
		StdioFDs:         stdio,
		GoferMountConfs:  mountConfs,
		PodInitConfigFD:  -1,
		ExecFD:           -1,
		RootfsUpperTarFD: -1,
		CPUDMALatencyFD:  -1,
	}
	l, err := New(args)
	if err != nil {
		cleanupGofers()
		return nil, nil, fmt.Errorf("boot.New: %w", err)
	}
	return l, cleanupGofers, nil
}

// TestTTCMaxTasks validates TTC-specific activation of sandbox-wide task limits.
func TestTTCMaxTasks(t *testing.T) {
	positive := &specs.LinuxPids{Limit: 7}
	nonPositive := &specs.LinuxPids{Limit: 0}
	for _, test := range []struct {
		name    string
		spec    *specs.Spec
		want    uint64
		wantErr bool
	}{
		{name: "nil spec"},
		{name: "ordinary OCI resources are not sandbox-wide", spec: &specs.Spec{Linux: &specs.Linux{Resources: &specs.LinuxResources{Pids: positive}}}},
		{name: "marker requires resources", spec: &specs.Spec{Annotations: map[string]string{ttcPidsLimitAnnotation: "sentry"}}, wantErr: true},
		{name: "marker requires positive limit", spec: &specs.Spec{Annotations: map[string]string{ttcPidsLimitAnnotation: "sentry"}, Linux: &specs.Linux{Resources: &specs.LinuxResources{Pids: nonPositive}}}, wantErr: true},
		{name: "TTC limit", spec: &specs.Spec{Annotations: map[string]string{ttcPidsLimitAnnotation: "sentry"}, Linux: &specs.Linux{Resources: &specs.LinuxResources{Pids: positive}}}, want: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ttcMaxTasks(test.spec)
			if (err != nil) != test.wantErr {
				t.Fatalf("ttcMaxTasks() error=%v, wantErr=%t", err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("ttcMaxTasks()=%d, want %d", got, test.want)
			}
		})
	}
}

// TestRun runs a simple application in a sandbox and checks that it succeeds.
func TestRun(t *testing.T) {
	l, cleanup, err := createLoader(testConfig(), testSpec())
	if err != nil {
		t.Fatalf("error creating loader: %v", err)
	}

	defer l.Destroy()
	defer cleanup()

	// Start a goroutine to read the start chan result, otherwise Run will
	// block forever.
	var resultChanErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		resultChanErr = <-l.ctrl.manager.startResultChan
		wg.Done()
	}()

	// Run the container.
	if err := l.Run(); err != nil {
		t.Errorf("error running container: %v", err)
	}

	// We should have not gotten an error on the startResultChan.
	wg.Wait()
	if resultChanErr != nil {
		t.Errorf("error on startResultChan: %v", resultChanErr)
	}

	// Wait for the application to exit.  It should succeed.
	if status := l.WaitExit(); !status.Exited() || status.ExitStatus() != 0 {
		t.Errorf("application exited with %s, want exit status 0", status)
	}
}

func TestInitialInternalPTYWinsize(t *testing.T) {
	for _, test := range []struct {
		rows, cols uint64
		want       linux.Winsize
	}{
		{37, 91, linux.Winsize{Row: 37, Col: 91}},
		{0, 0, linux.Winsize{Row: 24, Col: 80}},
		{37, 0, linux.Winsize{Row: 24, Col: 80}},
		{1 << 16, 80, linux.Winsize{Row: 24, Col: 80}},
	} {
		if got := initialInternalPTYWinsize(test.rows, test.cols); got != test.want {
			t.Errorf("initialInternalPTYWinsize(%d, %d)=%+v, want %+v", test.rows, test.cols, got, test.want)
		}
	}
}

// TestPreStartInternalTTYPushCapturesQuickExit verifies the console push that
// OCI create/start relies on: it registers before start, reads once the
// internal PTY exists, and a quick init exit loses none of its terminal output
// and ends the push with the init's status.
func TestPreStartInternalTTYPushCapturesQuickExit(t *testing.T) {
	spec := testSpec()
	spec.Process.Args = []string{"/bin/sh", "-c", "printf quick-terminal-output"}
	spec.Process.Terminal = true
	spec.Mounts = []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
	}
	const consoleID = "init"
	spec.Annotations = map[string]string{InternalConsoleAnnotation: consoleID}
	conf := testConfig()
	conf.DirectFS = false
	l, cleanup, err := createLoaderWithGoferMountConf(conf, spec, specutils.GoferMountConf{Lower: specutils.Lisafs, Upper: specutils.NoOverlay})
	if err != nil {
		t.Fatalf("creating terminal loader: %v", err)
	}
	defer cleanup()
	defer l.Destroy()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating console push pipe: %v", err)
	}
	defer reader.Close()
	if err := l.family.PushConsole(DefaultBranchID, l.root.cid, "", consoleID, writer); err != nil {
		t.Fatalf("registering the console push before start: %v", err)
	}
	pushed := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		pushed <- data
	}()

	startResult := make(chan error, 1)
	go func() { startResult <- <-l.ctrl.manager.startResultChan }()
	if err := l.Run(); err != nil {
		t.Fatalf("running quick terminal container: %v", err)
	}
	if err := <-startResult; err != nil {
		t.Fatalf("start result: %v", err)
	}
	var data []byte
	select {
	case data = <-pushed:
	case <-time.After(5 * time.Second):
		t.Fatal("console push did not end after the quick exit")
	}
	var output []byte
	exited := false
	for len(data) != 0 {
		if len(data) < ttcconsole.HeaderSize {
			t.Fatalf("pushed stream ends in a partial header: %x", data)
		}
		size := ttcconsole.HeaderSize + int(binary.BigEndian.Uint32(data[4:ttcconsole.HeaderSize]))
		if len(data) < size || exited {
			t.Fatalf("pushed stream has a partial frame or a frame after the exit: %x", data)
		}
		switch data[0] {
		case ConsolePushExit:
			if status := binary.BigEndian.Uint32(data[ttcconsole.HeaderSize:size]); status != 0 {
				t.Fatalf("pushed wait status %#x, want 0", status)
			}
			exited = true
		case ConsolePushError:
			t.Fatalf("console push failed: %s", data[ttcconsole.HeaderSize:size])
		default:
			frame, _, complete, err := ttcconsole.DecodeFrame(data[:size])
			if err != nil || !complete {
				t.Fatalf("decoding pushed frame: complete %t, %v", complete, err)
			}
			output = append(output, frame.Data...)
		}
		data = data[size:]
	}
	if !exited {
		t.Fatal("console push ended without the init's status")
	}
	if !bytes.Contains(output, []byte("quick-terminal-output")) {
		t.Fatalf("pushed output %q does not contain the quick exit's output", output)
	}
}

// TestRunInternalTTYLiveFork verifies that a terminal container uses a fully
// Sentry-owned PTY and that live fork duplicates both endpoints and queued data.
func TestRunInternalTTYLiveFork(t *testing.T) {
	spec := testSpec()
	spec.Process.Args = []string{"/bin/sleep", "30"}
	spec.Process.Terminal = true
	spec.Mounts = []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
	}
	const consoleID = "init"
	spec.Annotations = map[string]string{InternalConsoleAnnotation: consoleID}
	conf := testConfig()
	conf.DirectFS = false
	l, cleanup, err := createLoaderWithGoferMountConf(conf, spec, specutils.GoferMountConf{Lower: specutils.Lisafs, Upper: specutils.NoOverlay})
	if err != nil {
		t.Fatalf("creating terminal loader: %v", err)
	}
	defer cleanup()
	defer l.Destroy()

	if _, _, _, err := l.k.ReadInternalConsole(l.k.SupervisorContext(), consoleID, kernel.InternalConsoleStdout, 1); err == nil {
		t.Fatal("internal PTY unexpectedly exists before the container VFS is mounted")
	}
	resultCh := make(chan error, 1)
	go func() { resultCh <- <-l.ctrl.manager.startResultChan }()
	if err := l.Run(); err != nil {
		t.Fatalf("running terminal container: %v", err)
	}
	if err := <-resultCh; err != nil {
		t.Fatalf("start result: %v", err)
	}

	for index, stage := range []SandboxExecStage{
		SandboxExecStageNamespaceSelected,
		SandboxExecStageSessionReserved,
		SandboxExecStageConsoleCreated,
		SandboxExecStageProcessCreated,
		SandboxExecStageSessionPublished,
	} {
		execID := fmt.Sprintf("failed-exec-%d", index)
		consoleID := fmt.Sprintf("failed-console-%d", index)
		injected := errors.New("injected exec transaction failure")
		l.family.testOnlyExecFailureInjector = func(completed SandboxExecStage) error {
			if completed == stage {
				return injected
			}
			return nil
		}
		_, err := l.executeAsync(&control.ExecArgs{
			BranchID: DefaultBranchID, Filename: "/bin/sleep", Argv: []string{"/bin/sleep", "30"},
			Envv: []string{"PATH=/bin:/usr/bin"}, WorkingDirectory: "/",
			KUID: auth.RootKUID, KGID: auth.RootKGID, ExecID: execID,
			InternalConsoleID: consoleID, ContainerID: l.sandboxID,
		})
		l.family.testOnlyExecFailureInjector = nil
		if !errors.Is(err, injected) {
			t.Fatalf("exec failure at %s: got %v", stage, err)
		}
		if sessions, err := l.family.ExecSessions(&ExecSessionsArgs{BranchID: DefaultBranchID, CID: l.sandboxID}); err != nil {
			t.Fatalf("listing sessions after %s: %v", stage, err)
		} else if len(sessions) != 0 {
			t.Fatalf("sessions leaked after %s: %+v", stage, sessions)
		}
		if _, _, _, err := l.k.ReadInternalConsole(l.k.SupervisorContext(), consoleID, kernel.InternalConsoleStdout, 1); err == nil {
			t.Fatalf("internal console %q leaked after %s", consoleID, stage)
		}
	}

	const (
		transientExecID    = "transient-exec"
		transientConsoleID = "transient-console"
	)
	if _, err := l.executeAsync(&control.ExecArgs{
		BranchID: DefaultBranchID, Filename: "/bin/true", Argv: []string{"/bin/true"},
		Envv: []string{"PATH=/bin:/usr/bin"}, WorkingDirectory: "/",
		KUID: auth.RootKUID, KGID: auth.RootKGID, ExecID: transientExecID,
		InternalConsoleID: transientConsoleID, ContainerID: l.sandboxID,
	}); err != nil {
		t.Fatalf("executing transient session: %v", err)
	}
	if status, err := l.family.WaitExecSession(&ExecSessionArgs{BranchID: DefaultBranchID, ExecID: transientExecID}, nil); err != nil || status != 0 {
		t.Fatalf("waiting for transient session: status=%#x err=%v", status, err)
	}
	if sessions, err := l.family.ExecSessions(&ExecSessionsArgs{BranchID: DefaultBranchID, CID: l.sandboxID}); err != nil {
		t.Fatalf("listing sessions after transient wait: %v", err)
	} else if len(sessions) != 0 {
		t.Fatalf("transient session retained after wait: %+v", sessions)
	}
	if err := l.k.RemoveInternalConsole(l.k.SupervisorContext(), transientConsoleID); err != nil {
		t.Fatalf("removing transient console: %v", err)
	}

	stdinFile, err := os.Open("/dev/null")
	if err != nil {
		t.Fatalf("opening donated stdin: %v", err)
	}
	defer stdinFile.Close()
	outputFile, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening donated output: %v", err)
	}
	defer outputFile.Close()
	injectedImportFailure := errors.New("injected post-import failure")
	l.family.testOnlyExecFailureInjector = func(completed SandboxExecStage) error {
		if completed == SandboxExecStageFilesImported {
			return injectedImportFailure
		}
		return nil
	}
	_, err = l.executeAsync(&control.ExecArgs{
		Filename: "/bin/sleep", Argv: []string{"/bin/sleep", "30"},
		Envv: []string{"PATH=/bin:/usr/bin"}, WorkingDirectory: "/",
		KUID: auth.RootKUID, KGID: auth.RootKGID,
		ContainerID: l.sandboxID,
		FilePayload: control.NewFilePayload(map[int]*os.File{0: stdinFile, 1: outputFile, 2: outputFile}, nil),
	})
	l.family.testOnlyExecFailureInjector = nil
	if !errors.Is(err, injectedImportFailure) {
		t.Fatalf("donated-FD post-import failure: %v", err)
	}
	if sessions, err := l.family.ExecSessions(&ExecSessionsArgs{BranchID: DefaultBranchID, CID: l.sandboxID}); err != nil {
		t.Fatalf("listing sessions after donated-FD failure: %v", err)
	} else if len(sessions) != 0 {
		t.Fatalf("sessions leaked after donated-FD failure: %+v", sessions)
	}

	ctx := l.k.SupervisorContext()
	parentTask := l.k.GlobalInit().Leader()
	if parentTask.ThreadGroup().TTY() == nil {
		t.Fatal("terminal process has no controlling TTY")
	}
	parentStdin := liveForkTestFD(t, l.k, 0)
	parentStdout := liveForkTestFD(t, l.k, 1)
	parentStderr := liveForkTestFD(t, l.k, 2)
	if parentStdin.Impl() != parentStdout.Impl() || parentStdout.Impl() != parentStderr.Impl() {
		t.Fatal("terminal stdin, stdout, and stderr do not share one replica")
	}
	liveForkTestWrite(t, parentTask, parentStdout, "fork-output")
	if data, eof, wouldBlock, err := l.k.ReadInternalConsole(ctx, consoleID, kernel.InternalConsoleStderr, 1); err != nil || !eof || wouldBlock || len(data) != 0 {
		t.Fatalf("terminal stderr probe: got %q eof=%t wouldBlock=%t err=%v", data, eof, wouldBlock, err)
	}

	const (
		execSessionID = "exec-one"
		execConsoleID = "exec-console"
	)
	execPID, err := l.executeAsync(&control.ExecArgs{
		BranchID:          DefaultBranchID,
		Filename:          "/bin/sleep",
		Argv:              []string{"/bin/sleep", "30"},
		Envv:              []string{"PATH=/bin:/usr/bin"},
		WorkingDirectory:  "/",
		KUID:              auth.RootKUID,
		KGID:              auth.RootKGID,
		ExecID:            execSessionID,
		InternalConsoleID: execConsoleID,
		StdioIsPty:        true,
		ContainerID:       l.sandboxID,
	})
	if err != nil {
		t.Fatalf("executing process with an internal PTY: %v", err)
	}
	parentExec, err := branchThreadGroup(l.k, l.sandboxID, execPID)
	if err != nil {
		t.Fatalf("looking up parent exec process: %v", err)
	}
	parentExecTask := parentExec.Leader()
	parentExecStdin := liveForkTestTaskFD(t, parentExecTask, 0)
	parentExecStdout := liveForkTestTaskFD(t, parentExecTask, 1)
	parentExecStderr := liveForkTestTaskFD(t, parentExecTask, 2)
	if parentExec.TTY() == nil {
		t.Fatal("exec process has no controlling TTY")
	}
	if parentExec.TTY() == parentTask.ThreadGroup().TTY() {
		t.Fatal("init and exec processes share a controlling TTY")
	}
	if parentExecStdin.Impl() != parentExecStdout.Impl() || parentExecStdout.Impl() != parentExecStderr.Impl() {
		t.Fatal("exec terminal stdin, stdout, and stderr do not share one replica")
	}
	liveForkTestWrite(t, parentExecTask, parentExecStdout, "exec-fork-output")
	const (
		pipeExecSessionID = "pipe-exec"
		pipeExecConsoleID = "pipe-exec-console"
	)
	pipeExecPID, err := l.executeAsync(&control.ExecArgs{
		BranchID:          DefaultBranchID,
		Filename:          "/bin/sleep",
		Argv:              []string{"/bin/sleep", "30"},
		Envv:              []string{"PATH=/bin:/usr/bin"},
		WorkingDirectory:  "/",
		KUID:              auth.RootKUID,
		KGID:              auth.RootKGID,
		ExecID:            pipeExecSessionID,
		InternalConsoleID: pipeExecConsoleID,
		ContainerID:       l.sandboxID,
	})
	if err != nil {
		t.Fatalf("executing process with internal pipes: %v", err)
	}
	parentPipeExec, err := branchThreadGroup(l.k, l.sandboxID, pipeExecPID)
	if err != nil {
		t.Fatalf("looking up parent pipe exec process: %v", err)
	}
	parentPipeExecTask := parentPipeExec.Leader()
	parentPipeExecStdin := liveForkTestTaskFD(t, parentPipeExecTask, 0)
	parentPipeExecStdout := liveForkTestTaskFD(t, parentPipeExecTask, 1)
	parentPipeExecStderr := liveForkTestTaskFD(t, parentPipeExecTask, 2)
	if parentPipeExec.TTY() != nil {
		t.Fatal("pipe exec process unexpectedly has a controlling TTY")
	}
	if parentPipeExecStdin.Impl() == parentPipeExecStdout.Impl() || parentPipeExecStdout.Impl() == parentPipeExecStderr.Impl() || parentPipeExecStdin.Impl() == parentPipeExecStderr.Impl() {
		t.Fatal("pipe exec stdin, stdout, and stderr do not use distinct pipes")
	}
	liveForkTestWrite(t, parentPipeExecTask, parentPipeExecStdout, "pipe-fork-stdout")
	liveForkTestWrite(t, parentPipeExecTask, parentPipeExecStderr, "pipe-fork-stderr")
	liveForkTestExecSession(t, l.family, DefaultBranchID, l.sandboxID, execSessionID, execPID, true, ExecSessionRunning)
	liveForkTestExecSession(t, l.family, DefaultBranchID, l.sandboxID, pipeExecSessionID, pipeExecPID, false, ExecSessionRunning)
	if sessions, err := l.family.ExecSessions(&ExecSessionsArgs{BranchID: DefaultBranchID, CID: l.sandboxID}); err != nil {
		t.Fatalf("listing parent exec sessions: %v", err)
	} else if len(sessions) != 2 {
		t.Fatalf("parent exec session count: got %d, want 2", len(sessions))
	}
	var legacyExecIDs []execID
	l.mu.Lock()
	for id := range l.processes {
		if id.pid != 0 {
			legacyExecIDs = append(legacyExecIDs, id)
		}
	}
	l.mu.Unlock()
	if len(legacyExecIDs) != 0 {
		t.Fatalf("managed main-branch execs leaked into legacy Loader.processes: %+v", legacyExecIDs)
	}
	if _, err := l.executeAsync(&control.ExecArgs{
		BranchID:          DefaultBranchID,
		Filename:          "/bin/sleep",
		Argv:              []string{"/bin/sleep", "30"},
		Envv:              []string{"PATH=/bin:/usr/bin"},
		WorkingDirectory:  "/",
		KUID:              auth.RootKUID,
		KGID:              auth.RootKGID,
		ExecID:            execSessionID,
		InternalConsoleID: "duplicate-exec-console",
		ContainerID:       l.sandboxID,
	}); err == nil {
		t.Fatal("duplicate managed exec session ID was accepted")
	}

	childInfo, err := l.family.Fork(&ForkBranchArgs{ID: "tty-child"})
	if err != nil {
		t.Fatalf("forking terminal container: %v", err)
	}
	child, err := l.family.branch(childInfo.ID)
	if err != nil {
		t.Fatalf("looking up terminal child: %v", err)
	}
	childCtx := child.k.SupervisorContext()
	childTask := child.k.GlobalInit().Leader()
	childStdin := liveForkTestFD(t, child.k, 0)
	childStdout := liveForkTestFD(t, child.k, 1)
	childExec, err := branchThreadGroup(child.k, l.sandboxID, execPID)
	if err != nil {
		t.Fatalf("looking up child exec process: %v", err)
	}
	childExecTask := childExec.Leader()
	childExecStdin := liveForkTestTaskFD(t, childExecTask, 0)
	childExecStdout := liveForkTestTaskFD(t, childExecTask, 1)
	if parentExecStdout.Impl() == childExecStdout.Impl() {
		t.Fatal("parent and child share the internal exec PTY replica object")
	}
	if parentExec.TTY() == childExec.TTY() {
		t.Fatal("parent and child share the exec controlling TTY object")
	}
	childPipeExec, err := branchThreadGroup(child.k, l.sandboxID, pipeExecPID)
	if err != nil {
		t.Fatalf("looking up child pipe exec process: %v", err)
	}
	childPipeExecTask := childPipeExec.Leader()
	childPipeExecStdin := liveForkTestTaskFD(t, childPipeExecTask, 0)
	childPipeExecStdout := liveForkTestTaskFD(t, childPipeExecTask, 1)
	childPipeExecStderr := liveForkTestTaskFD(t, childPipeExecTask, 2)
	if childPipeExec.TTY() != nil {
		t.Fatal("forked pipe exec process unexpectedly has a controlling TTY")
	}
	if parentPipeExecStdout.Impl() == childPipeExecStdout.Impl() || parentPipeExecStderr.Impl() == childPipeExecStderr.Impl() {
		t.Fatal("parent and child share internal exec pipe objects")
	}
	liveForkTestExecSession(t, l.family, childInfo.ID, l.sandboxID, execSessionID, execPID, true, ExecSessionRunning)
	liveForkTestExecSession(t, l.family, childInfo.ID, l.sandboxID, pipeExecSessionID, pipeExecPID, false, ExecSessionRunning)
	if sessions, err := l.family.ExecSessions(&ExecSessionsArgs{BranchID: childInfo.ID, CID: l.sandboxID}); err != nil {
		t.Fatalf("listing child exec sessions: %v", err)
	} else if len(sessions) != 2 {
		t.Fatalf("child exec session count: got %d, want 2", len(sessions))
	}

	if parentStdout.Impl() == childStdout.Impl() {
		t.Fatal("parent and child share the internal PTY replica object")
	}
	if parentTask.ThreadGroup().TTY() == childTask.ThreadGroup().TTY() {
		t.Fatal("parent and child share the controlling TTY object")
	}
	liveForkTestConsoleRead(t, l.k, ctx, consoleID, kernel.InternalConsoleStdout, "fork-output")
	liveForkTestConsoleRead(t, child.k, childCtx, consoleID, kernel.InternalConsoleStdout, "fork-output")
	liveForkTestWrite(t, childTask, childStdout, "child-output")
	liveForkTestConsoleRead(t, child.k, childCtx, consoleID, kernel.InternalConsoleStdout, "child-output")
	liveForkTestConsoleWouldBlock(t, l.k, ctx, consoleID, kernel.InternalConsoleStdout)

	liveForkTestConsoleRead(t, l.k, ctx, execConsoleID, kernel.InternalConsoleStdout, "exec-fork-output")
	liveForkTestConsoleRead(t, child.k, childCtx, execConsoleID, kernel.InternalConsoleStdout, "exec-fork-output")
	liveForkTestWrite(t, childExecTask, childExecStdout, "exec-child-output")
	liveForkTestConsoleRead(t, child.k, childCtx, execConsoleID, kernel.InternalConsoleStdout, "exec-child-output")
	liveForkTestConsoleWouldBlock(t, l.k, ctx, execConsoleID, kernel.InternalConsoleStdout)
	liveForkTestConsoleWriteInput(t, child.k, childCtx, execConsoleID, "exec-child-input\n")
	liveForkTestRead(t, childExecTask, childExecStdin, "exec-child-input\n")
	if got := parentExecStdin.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Fatalf("child exec PTY input became readable in parent: readiness=%#x", got)
	}
	liveForkTestConsoleRead(t, l.k, ctx, pipeExecConsoleID, kernel.InternalConsoleStdout, "pipe-fork-stdout")
	liveForkTestConsoleRead(t, child.k, childCtx, pipeExecConsoleID, kernel.InternalConsoleStdout, "pipe-fork-stdout")
	liveForkTestConsoleRead(t, l.k, ctx, pipeExecConsoleID, kernel.InternalConsoleStderr, "pipe-fork-stderr")
	liveForkTestConsoleRead(t, child.k, childCtx, pipeExecConsoleID, kernel.InternalConsoleStderr, "pipe-fork-stderr")
	liveForkTestWrite(t, childPipeExecTask, childPipeExecStdout, "pipe-child-output")
	liveForkTestConsoleRead(t, child.k, childCtx, pipeExecConsoleID, kernel.InternalConsoleStdout, "pipe-child-output")
	liveForkTestConsoleWouldBlock(t, l.k, ctx, pipeExecConsoleID, kernel.InternalConsoleStdout)
	liveForkTestConsoleWriteInput(t, child.k, childCtx, pipeExecConsoleID, "pipe-child-input")
	liveForkTestRead(t, childPipeExecTask, childPipeExecStdin, "pipe-child-input")
	if got := parentPipeExecStdin.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Fatalf("child exec pipe input became readable in parent: readiness=%#x", got)
	}

	liveForkTestConsoleWriteInput(t, child.k, childCtx, consoleID, "child-input\n")
	liveForkTestRead(t, childTask, childStdin, "child-input\n")
	if got := parentStdin.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Fatalf("child PTY input became readable in parent: readiness=%#x", got)
	}

	manager := &containerManager{l: l}
	livenessReader, livenessWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating exec wait liveness pipe: %v", err)
	}
	defer livenessReader.Close()
	cancelledWait := make(chan error, 1)
	go func() {
		var status uint32
		cancelledWait <- manager.ExecWait(&ExecWaitArgs{
			FilePayload:     urpc.FilePayload{Files: []*os.File{livenessReader}},
			ExecSessionArgs: ExecSessionArgs{BranchID: childInfo.ID, ExecID: execSessionID},
		}, &status)
	}()
	select {
	case err := <-cancelledWait:
		t.Fatalf("exec wait returned before cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := livenessWriter.Close(); err != nil {
		t.Fatalf("closing exec wait liveness pipe: %v", err)
	}
	select {
	case err := <-cancelledWait:
		if !errors.Is(err, ErrExecWaitCancelled) {
			t.Fatalf("cancelled exec wait: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cancelled exec wait retained its control lease")
	}
	if err := l.family.Pause(childInfo.ID); err != nil {
		t.Fatalf("pausing child after exec wait cancellation: %v", err)
	}
	if err := l.family.Resume(childInfo.ID); err != nil {
		t.Fatalf("resuming child after exec wait cancellation: %v", err)
	}

	if err := manager.Signal(&SignalArgs{BranchID: childInfo.ID, CID: l.sandboxID, Signo: int32(linux.SIGWINCH), Mode: DeliverToForegroundProcessGroup}, &struct{}{}); err != nil {
		t.Fatalf("signaling child foreground process group: %v", err)
	}
	if err := manager.Signal(&SignalArgs{BranchID: childInfo.ID, CID: l.sandboxID, PID: int32(execPID), Signo: int32(linux.SIGKILL), Mode: DeliverToProcess}, &struct{}{}); err != nil {
		t.Fatalf("signaling child exec through general RPC: %v", err)
	}
	status, err := l.family.WaitExecSession(&ExecSessionArgs{BranchID: childInfo.ID, ExecID: execSessionID}, nil)
	if err != nil {
		t.Fatalf("waiting for child exec session: %v", err)
	}
	if want := uint32(linux.WaitStatusTerminationSignal(linux.SIGKILL)); status != want {
		t.Fatalf("child exec wait status: got %#x, want %#x", status, want)
	}
	exitedSession := liveForkTestExecSession(t, l.family, childInfo.ID, l.sandboxID, execSessionID, execPID, true, ExecSessionExited)
	if exitedSession.ExitStatus == nil || *exitedSession.ExitStatus != status {
		t.Fatalf("child exec retained status: got %v, want %#x", exitedSession.ExitStatus, status)
	}
	liveForkTestExecSession(t, l.family, DefaultBranchID, l.sandboxID, execSessionID, execPID, true, ExecSessionRunning)

	if err := l.family.Destroy(childInfo.ID); err != nil {
		t.Fatalf("destroying terminal child: %v", err)
	}
	l.k.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	if l.k.IsPaused() {
		l.k.Unpause()
	}
	l.k.WaitExited()
}

func liveForkTestExecSession(t *testing.T, family *SandboxFamily, branchID, cid, id string, pid kernel.ThreadID, terminal bool, state ExecSessionState) ExecSessionInfo {
	t.Helper()
	sessions, err := family.ExecSessions(&ExecSessionsArgs{BranchID: branchID, CID: cid})
	if err != nil {
		t.Fatalf("listing exec sessions in branch %q: %v", branchID, err)
	}
	for _, session := range sessions {
		if session.ID != id {
			continue
		}
		if session.ContainerID != cid || session.PID != pid || session.ConsoleID == "" || session.Terminal != terminal || session.State != state {
			t.Fatalf("exec session %q in branch %q: got %+v, want cid=%q pid=%d terminal=%t state=%q", id, branchID, session, cid, pid, terminal, state)
		}
		if state != ExecSessionExited && session.ExitStatus != nil {
			t.Fatalf("running exec session %q unexpectedly has exit status %#x", id, *session.ExitStatus)
		}
		return session
	}
	t.Fatalf("exec session %q missing from branch %q: %+v", id, branchID, sessions)
	return ExecSessionInfo{}
}

func TestRestoredExecSessionsRetainsExitedMetadata(t *testing.T) {
	status := uint32(linux.WaitStatusExit(7))
	sessions, err := restoredExecSessions(nil, []ExecSessionInfo{{
		ID: "finished", ContainerID: "container", PID: 42, ConsoleID: "console-finished",
		State: ExecSessionExited, ExitStatus: &status,
	}})
	if err != nil {
		t.Fatal(err)
	}
	session := sessions["finished"]
	if session == nil || session.tg != nil || !session.retain || session.info.ExitStatus == nil || *session.info.ExitStatus != status {
		t.Fatalf("restored exited session=%+v", session)
	}
	*session.info.ExitStatus = 99
	if status != uint32(linux.WaitStatusExit(7)) {
		t.Fatal("restored exit status aliases serialized metadata")
	}

	// A remote restore installs both running and completed records, then asks
	// the family to start watchers for the resulting map. Completed records
	// have no ThreadGroup and must remain passive retained metadata.
	family := &SandboxFamily{}
	family.watchExecSession("branch", "finished", session)
	time.Sleep(10 * time.Millisecond)
	if session.info.State != ExecSessionExited || session.info.ExitStatus == nil || *session.info.ExitStatus != 99 {
		t.Fatalf("watching completed session changed metadata: %+v", session.info)
	}
}

// TestRunLiveForkNetworkNone forks a live loader with no host-facing network.
func TestRunLiveForkNetworkNone(t *testing.T) {
	hostBind := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostBind, "fixture"), []byte("host-data"), 0644); err != nil {
		t.Fatalf("writing read-only bind fixture: %v", err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(hostBind, name), []byte(name), 0644); err != nil {
			t.Fatalf("writing read-only directory fixture %q: %v", name, err)
		}
	}
	spec := testSpec()
	spec.Process.Args = []string{"/bin/sleep", "30"}
	// The Bazel sandbox may expose the host root through an unprivileged user
	// namespace, in which case lisafs reports it as owned by nobody. Model a
	// normal root container explicitly so the writable memory overlay is tested
	// independently of the host test sandbox's UID mapping.
	rootCaps := []string{"CAP_DAC_OVERRIDE"}
	spec.Process.Capabilities = &specs.LinuxCapabilities{
		Bounding:  rootCaps,
		Effective: rootCaps,
		Permitted: rootCaps,
	}
	spec.Root.Readonly = false
	spec.Mounts = []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/readonly", Type: "bind", Source: hostBind, Options: []string{"rbind", "ro"}},
		{Destination: "/managed", Type: "tmpfs", Source: "ttc-volume-cache", Options: []string{"rw", "nosuid", "nodev", "mode=0755"}},
		{Destination: "/managed-alias", Type: "tmpfs", Source: "ttc-volume-cache", Options: []string{"rw", "nosuid", "nodev", "mode=0755"}},
	}
	spec.Annotations = map[string]string{
		InternalConsoleAnnotation:               "init",
		"dev.gvisor.spec.mount.managed.source":  "ttc-volume-cache",
		"dev.gvisor.spec.mount.managed.share":   "container",
		"dev.gvisor.spec.mount.managed.type":    "tmpfs",
		"dev.gvisor.spec.mount.managed.options": "rw,nosuid,nodev,mode=0755",
	}
	conf := testConfig()
	conf.DirectFS = false
	l, cleanup, err := createLoaderWithGoferMountConfs(conf, spec, []specutils.GoferMountConf{
		{Lower: specutils.Lisafs, Upper: specutils.MemoryOverlay},
		{Lower: specutils.Lisafs, Upper: specutils.NoOverlay},
	})
	if err != nil {
		t.Fatalf("error creating loader: %v", err)
	}
	defer cleanup()
	defer l.Destroy()
	// OCI create must expose the console before StartRoot can run guest code.
	liveForkTestConsoleWouldBlock(t, l.k, l.k.SupervisorContext(), "init", kernel.InternalConsoleStdout)
	type waitOutcome struct {
		status uint32
		err    error
	}
	preStartWait := make(chan waitOutcome, 1)
	go func() {
		status, err := l.family.Wait(DefaultBranchID, l.root.cid, 0)
		preStartWait <- waitOutcome{status: status, err: err}
	}()
	select {
	case result := <-preStartWait:
		t.Fatalf("wait established before Loader.Run returned early: %+v", result)
	case <-time.After(10 * time.Millisecond):
		// The waiter must remain in flight until Loader.Run installs init.
	}

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- <-l.ctrl.manager.startResultChan
	}()
	if err := l.Run(); err != nil {
		t.Fatalf("running container: %v", err)
	}
	if err := <-resultCh; err != nil {
		t.Fatalf("start result: %v", err)
	}

	var sharedExternalMounts int
	filesystemRecords := l.k.VFS().LiveForkFilesystemEligibilities(l.k.SupervisorContext())
	for _, eligibility := range filesystemRecords {
		switch eligibility.Decision {
		case vfs.LiveForkFilesystemClone:
		case vfs.LiveForkFilesystemShareExternal:
			sharedExternalMounts++
			if !eligibility.ReadOnly {
				t.Errorf("shared live-fork mount is writable: %+v", eligibility)
			}
			if strings.HasPrefix(eligibility.StableIdentity, "mount:") {
				t.Errorf("shared live-fork mount has only fallback identity: %+v", eligibility)
			}
		case vfs.LiveForkFilesystemReject:
			t.Errorf("live-fork mount is unclassified or rejected: %+v", eligibility)
		default:
			t.Errorf("live-fork mount has invalid decision: %+v", eligibility)
		}
	}
	if sharedExternalMounts != 2 {
		t.Errorf("shared external live-fork mounts: got %d, want root lower and read-only bind", sharedExternalMounts)
	}

	// The annotated init stdio is entirely Sentry-owned and remains forkable.
	l.k.Pause()
	ctx := l.k.SupervisorContext()
	parentTask := l.k.GlobalInit().Leader()
	l.family.allowSharedFilesystemResources = false
	_, err = l.family.Fork(&ForkBranchArgs{ID: "shared-filesystem-rejected"})
	if err == nil {
		t.Fatal("live fork unexpectedly accepted shared external filesystems")
	}
	var sharedResourceErr *kernel.LiveForkResourceError
	if !errors.As(err, &sharedResourceErr) || len(sharedResourceErr.Issues) != 2 {
		t.Fatalf("shared-filesystem rejection: %v", err)
	}
	sharedPaths := map[string]bool{"": false, "/readonly": false}
	for _, issue := range sharedResourceErr.Issues {
		if issue.PID != 0 || issue.FD != -1 || issue.FilesystemType != "9p" || issue.SourceClass != "external-gofer" || !strings.Contains(issue.Resource, "filesystem \"9p\"") || !strings.Contains(issue.Reason, "not authorized") {
			t.Errorf("shared-filesystem rejection issue: %+v", issue)
		}
		if _, ok := sharedPaths[issue.MountPath]; !ok {
			t.Errorf("unexpected shared-filesystem mount path %q: %+v", issue.MountPath, issue)
		} else {
			sharedPaths[issue.MountPath] = true
		}
	}
	for path, seen := range sharedPaths {
		if !seen {
			t.Errorf("missing shared-filesystem rejection path %q", path)
		}
	}
	manifestSharedPaths := map[string]bool{"": false, "/readonly": false}
	for _, record := range filesystemRecords {
		if record.Decision != vfs.LiveForkFilesystemShareExternal {
			continue
		}
		if _, ok := manifestSharedPaths[record.MountPath]; ok {
			manifestSharedPaths[record.MountPath] = true
		}
	}
	for path, seen := range manifestSharedPaths {
		if !seen {
			t.Errorf("filesystem classification omitted shared path %q: %+v", path, filesystemRecords)
		}
	}
	for _, branch := range l.family.List() {
		if branch.ID == "shared-filesystem-rejected" {
			t.Fatal("rejected fork left a destination branch")
		}
	}
	l.family.allowSharedFilesystemResources = true

	mounts := l.k.VFS().GetAllMounts(ctx)
	var sysMount *vfs.Mount
	for _, mount := range mounts {
		if mount.Filesystem().FilesystemType().Name() == "sysfs" {
			sysMount = mount
		} else {
			mount.DecRef(ctx)
		}
	}
	if sysMount == nil {
		t.Fatal("restricted sysfs mount is missing")
	}
	if err := l.k.VFS().SetMountReadOnly(sysMount, false); err != nil {
		sysMount.DecRef(ctx)
		t.Fatalf("temporarily making sysfs writable: %v", err)
	}
	_, err = l.family.Fork(&ForkBranchArgs{ID: "writable-sysfs-rejected"})
	if err == nil {
		t.Fatal("live fork unexpectedly accepted writable sysfs")
	}
	var sysfsResourceErr *kernel.LiveForkResourceError
	if !errors.As(err, &sysfsResourceErr) || len(sysfsResourceErr.Issues) != 1 {
		t.Fatalf("writable sysfs rejection: %v", err)
	}
	sysfsIssue := sysfsResourceErr.Issues[0]
	if sysfsIssue.MountPath != "/sys" || sysfsIssue.FilesystemType != "sysfs" || sysfsIssue.SourceClass != "sentry-owned" || !strings.Contains(sysfsIssue.Resource, "sysfs") || !strings.Contains(sysfsIssue.Reason, "read-only mount") {
		t.Fatalf("writable sysfs rejection issue: %+v", sysfsIssue)
	}
	if err := l.k.VFS().SetMountReadOnly(sysMount, true); err != nil {
		sysMount.DecRef(ctx)
		t.Fatalf("restoring read-only sysfs: %v", err)
	}
	sysMount.DecRef(ctx)

	networkStack := l.k.RootNetworkNamespace().Stack().(*sentrynetstack.Stack)
	if err := (&Network{Stack: networkStack.Stack, Kernel: l.k}).CreateLinksAndRoutes(&CreateLinksAndRoutesArgs{
		LoopbackLinks: []LoopbackLink{DefaultLoopbackLink},
	}, &struct{}{}); err != nil {
		t.Fatalf("configuring contained loopback network: %v", err)
	}
	aioID, err := parentTask.MemoryManager().NewAIOContext(parentTask, 1)
	if err != nil {
		t.Fatalf("creating AIO ring mapping: %v", err)
	}
	l.k.Unpause()
	if _, err := l.family.Fork(&ForkBranchArgs{ID: "aio-ring"}); err == nil {
		t.Fatal("live fork unexpectedly accepted an AIO ring mapping")
	} else {
		var resourceErr *kernel.LiveForkResourceError
		if !errors.As(err, &resourceErr) {
			t.Fatalf("AIO ring rejection is not a resource report: %v", err)
		}
		if len(resourceErr.Issues) != 1 {
			t.Fatalf("AIO ring rejection issues: got %+v, want one", resourceErr.Issues)
		}
		issue := resourceErr.Issues[0]
		if issue.PID != 1 || issue.FD != -1 || !strings.Contains(issue.Resource, "aioMappable") || !strings.Contains(issue.Reason, "virtual address range") {
			t.Fatalf("AIO ring rejection issue: got %+v", issue)
		}
	}
	if l.k.IsPaused() {
		t.Fatal("running source remained paused after rejected live fork")
	}
	for _, branch := range l.family.List() {
		if branch.ID == "aio-ring" {
			t.Fatal("rejected live fork left a destination branch")
		}
	}
	l.k.Pause()
	if aio := parentTask.MemoryManager().DestroyAIOContext(parentTask, aioID); aio == nil {
		t.Fatal("AIO ring mapping disappeared after rejected live fork")
	}
	hostEventFD, err := createHostEventFDForTest()
	if err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip("host eventfd import is unavailable on this host")
		}
		t.Fatalf("creating host eventfd: %v", err)
	}
	hostEventFile, err := eventfd.NewFromHost(ctx, l.k.VFS(), hostEventFD, linux.O_RDWR|linux.O_NONBLOCK)
	if err != nil {
		unix.Close(hostEventFD)
		t.Fatalf("importing host eventfd: %v", err)
	}
	const rejectedFD = int32(9)
	if _, err := l.k.GlobalInit().Leader().FDTable().NewFDAt(ctx, rejectedFD, hostEventFile, kernel.FDFlags{}); err != nil {
		hostEventFile.DecRef(ctx)
		t.Fatalf("installing host eventfd: %v", err)
	}
	hostEventFile.DecRef(ctx)
	if _, err := l.family.Fork(&ForkBranchArgs{ID: "host-eventfd"}); err == nil {
		t.Fatal("live fork unexpectedly accepted a host-backed eventfd")
	} else {
		var resourceErr *kernel.LiveForkResourceError
		if !errors.As(err, &resourceErr) {
			t.Fatalf("host eventfd rejection is not a resource report: %v", err)
		}
		if len(resourceErr.Issues) != 1 || resourceErr.Issues[0].PID != 1 || resourceErr.Issues[0].FD != rejectedFD {
			t.Fatalf("host eventfd rejection issues: got %+v, want pid 1 fd %d", resourceErr.Issues, rejectedFD)
		}
	}
	rejectedEventFile := l.k.GlobalInit().Leader().FDTable().Remove(ctx, rejectedFD)
	if rejectedEventFile == nil {
		t.Fatalf("host eventfd %d disappeared", rejectedFD)
	}
	rejectionEpollFile, err := l.k.VFS().NewEpollInstanceFD(ctx)
	if err != nil {
		rejectedEventFile.DecRef(ctx)
		t.Fatalf("creating rejection epoll: %v", err)
	}
	rejectionEpoll := rejectionEpollFile.Impl().(*vfs.EpollInstance)
	if err := rejectionEpoll.AddInterest(rejectedEventFile, rejectedFD, linux.EpollEvent{Events: linux.EPOLLIN}); err != nil {
		rejectionEpollFile.DecRef(ctx)
		rejectedEventFile.DecRef(ctx)
		t.Fatalf("registering host eventfd with rejection epoll: %v", err)
	}
	const rejectionEpollFD = int32(8)
	if _, err := l.k.GlobalInit().Leader().FDTable().NewFDAt(ctx, rejectionEpollFD, rejectionEpollFile, kernel.FDFlags{}); err != nil {
		rejectionEpollFile.DecRef(ctx)
		rejectedEventFile.DecRef(ctx)
		t.Fatalf("installing rejection epoll: %v", err)
	}
	rejectionEpollFile.DecRef(ctx)
	if _, err := l.family.Fork(&ForkBranchArgs{ID: "epoll-host-eventfd"}); err == nil {
		t.Fatal("live fork unexpectedly accepted an epoll-reachable host eventfd")
	} else {
		var resourceErr *kernel.LiveForkResourceError
		if !errors.As(err, &resourceErr) {
			t.Fatalf("epoll host eventfd rejection is not a resource report: %v", err)
		}
		if len(resourceErr.Issues) != 1 || resourceErr.Issues[0].FD != rejectionEpollFD {
			t.Fatalf("epoll host eventfd rejection issues: got %+v, want root fd %d", resourceErr.Issues, rejectionEpollFD)
		}
	}
	if file := l.k.GlobalInit().Leader().FDTable().Remove(ctx, rejectionEpollFD); file != nil {
		file.DecRef(ctx)
	}
	rejectedEventFile.DecRef(ctx)
	pipeReader, pipeWriter, err := pipefs.NewConnectedPipeFDs(ctx, l.k.PipeMount(), linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating internal pipe: %v", err)
	}
	pipeFDs, err := l.k.GlobalInit().Leader().FDTable().NewFDs(ctx, 10, []*vfs.FileDescription{pipeReader, pipeWriter}, kernel.FDFlags{})
	pipeReader.DecRef(ctx)
	pipeWriter.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing internal pipe: %v", err)
	}
	parentReader := liveForkTestFD(t, l.k, pipeFDs[0])
	parentWriter := liveForkTestFD(t, l.k, pipeFDs[1])
	liveForkTestWrite(t, ctx, parentWriter, "seed")
	eventFile, err := eventfd.New(ctx, l.k.VFS(), 7, false, linux.O_RDWR|linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating internal eventfd: %v", err)
	}
	eventFD, err := l.k.GlobalInit().Leader().FDTable().NewFD(ctx, 12, eventFile, kernel.FDFlags{})
	eventFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing internal eventfd: %v", err)
	}
	parentEvent := liveForkTestFD(t, l.k, eventFD)
	epollFile, err := l.k.VFS().NewEpollInstanceFD(ctx)
	if err != nil {
		t.Fatalf("creating epoll instance: %v", err)
	}
	parentEpoll := epollFile.Impl().(*vfs.EpollInstance)
	const epollData = int32(0x544f53)
	if err := parentEpoll.AddInterest(parentReader, pipeFDs[0], linux.EpollEvent{
		Events: linux.EPOLLIN,
		Data:   [2]int32{epollData, 0},
	}); err != nil {
		t.Fatalf("registering pipe with epoll: %v", err)
	}
	epollFD, err := l.k.GlobalInit().Leader().FDTable().NewFD(ctx, 13, epollFile, kernel.FDFlags{})
	epollFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing epoll instance: %v", err)
	}
	parentEpollFile := liveForkTestFD(t, l.k, epollFD)
	parentEpoll = parentEpollFile.Impl().(*vfs.EpollInstance)
	timerFile, err := timerfd.New(ctx, l.k.VFS(), l.k.MonotonicClock(), linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating timerfd: %v", err)
	}
	parentTimer := timerFile.Impl().(*timerfd.TimerFileDescription)
	parentTimer.NotifyTimer(2)
	timerFD, err := l.k.GlobalInit().Leader().FDTable().NewFD(ctx, 14, timerFile, kernel.FDFlags{})
	timerFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing timerfd: %v", err)
	}
	parentTimerFile := liveForkTestFD(t, l.k, timerFD)
	parentTimer = parentTimerFile.Impl().(*timerfd.TimerFileDescription)
	shmSegment, err := parentTask.IPCNamespace().ShmRegistry().FindOrCreate(parentTask, 1, 0, hostarch.PageSize, 0600, true, true, true)
	if err != nil {
		t.Fatalf("creating SysV SHM segment: %v", err)
	}
	shmOpts, err := shmSegment.ConfigureAttach(parentTask, 0, shm.AttachOpts{})
	if err != nil {
		shmSegment.DecRef(parentTask)
		t.Fatalf("configuring first SysV SHM mapping: %v", err)
	}
	shmAddr1, err := parentTask.MemoryManager().MMap(parentTask, shmOpts)
	if err != nil {
		shmSegment.DecRef(parentTask)
		t.Fatalf("mapping first SysV SHM alias: %v", err)
	}
	shmOpts, err = shmSegment.ConfigureAttach(parentTask, 0, shm.AttachOpts{})
	if err != nil {
		shmSegment.DecRef(parentTask)
		t.Fatalf("configuring second SysV SHM mapping: %v", err)
	}
	shmAddr2, err := parentTask.MemoryManager().MMap(parentTask, shmOpts)
	if err != nil {
		shmSegment.DecRef(parentTask)
		t.Fatalf("mapping second SysV SHM alias: %v", err)
	}
	shmSegment.DecRef(parentTask)
	liveForkTestMMWrite(t, parentTask, shmAddr1, "seed")
	liveForkTestMMRead(t, parentTask, shmAddr2, "seed")
	signalMask := linux.SignalSetOf(linux.SIGUSR1)
	parentTask.SetSignalMask(parentTask.SignalMask() | signalMask)
	signalFile, err := signalfd.New(l.k.VFS(), parentTask, signalMask, linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating signalfd: %v", err)
	}
	signalFD, err := parentTask.FDTable().NewFD(ctx, 15, signalFile, kernel.FDFlags{})
	signalFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing signalfd: %v", err)
	}
	parentSignalFile := liveForkTestFD(t, l.k, signalFD)
	if err := parentTask.SendSignal(kernel.SignalInfoPriv(linux.SIGUSR1)); err != nil {
		t.Fatalf("queuing parent signal before fork: %v", err)
	}
	parentRoot := parentTask.MountNamespace().Root(ctx)
	inotifyFile, err := vfs.NewInotifyFD(ctx, l.k.VFS(), linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating inotify instance: %v", err)
	}
	parentInotify := inotifyFile.Impl().(*vfs.Inotify)
	watchID := parentInotify.AddWatch(parentRoot.Dentry(), linux.IN_ATTRIB)
	inotifyFD, err := parentTask.FDTable().NewFD(ctx, 16, inotifyFile, kernel.FDFlags{})
	inotifyFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing inotify instance: %v", err)
	}
	parentInotifyFile := liveForkTestFD(t, l.k, inotifyFD)
	parentRoot.Dentry().Watches().Notify(ctx, "", linux.IN_ATTRIB, 0, vfs.InodeEvent, false)
	unixEP1, unixEP2 := unixtransport.NewPair(parentTask, linux.SOCK_SEQPACKET, l.k)
	unixFile1, unixErr := unixsocket.NewSockfsFile(parentTask, unixEP1, linux.SOCK_SEQPACKET)
	if unixErr != nil {
		t.Fatalf("creating first internal Unix socket: %v", unixErr)
	}
	unixFile2, unixErr := unixsocket.NewSockfsFile(parentTask, unixEP2, linux.SOCK_SEQPACKET)
	if unixErr != nil {
		unixFile1.DecRef(parentTask)
		t.Fatalf("creating second internal Unix socket: %v", unixErr)
	}
	unixFDs, err := parentTask.FDTable().NewFDs(parentTask, 17, []*vfs.FileDescription{unixFile1, unixFile2}, kernel.FDFlags{})
	unixFile1.DecRef(parentTask)
	unixFile2.DecRef(parentTask)
	if err != nil {
		t.Fatalf("installing internal Unix socketpair: %v", err)
	}
	parentUnix1 := liveForkTestFD(t, l.k, unixFDs[0])
	parentUnix2 := liveForkTestFD(t, l.k, unixFDs[1])
	liveForkTestWrite(t, parentTask, parentUnix1, "seed")
	udpClientFile, udpServerFile := liveForkTestUDPPair(t, parentTask, 31001, 31002)
	const udpClientFD = int32(22)
	udpFDs, err := parentTask.FDTable().NewFDs(parentTask, udpClientFD, []*vfs.FileDescription{udpClientFile, udpServerFile}, kernel.FDFlags{})
	udpClientFile.DecRef(parentTask)
	udpServerFile.DecRef(parentTask)
	if err != nil {
		t.Fatalf("installing loopback UDP pair: %v", err)
	}
	parentUDPClient := liveForkTestFD(t, l.k, udpFDs[0])
	parentUDPServer := liveForkTestFD(t, l.k, udpFDs[1])
	liveForkTestWrite(t, parentTask, parentUDPClient, "seed")
	tcpClientFile, tcpServerFile := liveForkTestTCPPair(t, parentTask, 31003)
	const tcpClientFD = int32(24)
	tcpFDs, err := parentTask.FDTable().NewFDs(parentTask, tcpClientFD, []*vfs.FileDescription{tcpClientFile, tcpServerFile}, kernel.FDFlags{})
	tcpClientFile.DecRef(parentTask)
	tcpServerFile.DecRef(parentTask)
	if err != nil {
		t.Fatalf("installing loopback TCP pair: %v", err)
	}
	parentTCPClient := liveForkTestFD(t, l.k, tcpFDs[0])
	parentTCPServer := liveForkTestFD(t, l.k, tcpFDs[1])
	liveForkTestWrite(t, parentTask, parentTCPClient, "seed")
	transientNetworkWaiter, _ := waiter.NewTransientChannelEntry(waiter.ReadableEvents | waiter.EventErr | waiter.EventHUp)
	if err := parentTCPClient.EventRegister(&transientNetworkWaiter); err != nil {
		t.Fatalf("registering transient network waiter: %v", err)
	}
	netlinkFile, netlinkErr := socket.New(parentTask, linux.AF_NETLINK, linux.SOCK_RAW, linux.NETLINK_ROUTE)
	if netlinkErr != nil {
		t.Fatalf("creating netlink route socket: %v", netlinkErr)
	}
	const netlinkFD = int32(33)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, netlinkFD, netlinkFile, kernel.FDFlags{}); err != nil {
		netlinkFile.DecRef(parentTask)
		t.Fatalf("installing netlink route socket: %v", err)
	}
	netlinkFile.DecRef(parentTask)
	parentNetlink := liveForkTestFD(t, l.k, netlinkFD)
	memfdFile, err := tmpfs.NewMemfd(parentTask, parentTask.Credentials(), l.k.ShmMount(), false, "live-fork")
	if err != nil {
		t.Fatalf("creating memfd: %v", err)
	}
	const memfdFD = int32(19)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, memfdFD, memfdFile, kernel.FDFlags{}); err != nil {
		memfdFile.DecRef(parentTask)
		t.Fatalf("installing memfd: %v", err)
	}
	memfdFile.DecRef(parentTask)
	parentMemfd := liveForkTestFD(t, l.k, memfdFD)
	liveForkTestPWrite(t, parentTask, parentMemfd, 0, "seed")
	const overlayPath = "timetravel-overlay-state"
	overlayFile := liveForkTestCreateFile(t, parentTask, overlayPath)
	const overlayFD = int32(20)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, overlayFD, overlayFile, kernel.FDFlags{}); err != nil {
		overlayFile.DecRef(parentTask)
		t.Fatalf("installing overlay file: %v", err)
	}
	overlayFile.DecRef(parentTask)
	parentOverlayFile := liveForkTestFD(t, l.k, overlayFD)
	liveForkTestPWrite(t, parentTask, parentOverlayFile, 0, "seed")
	hardlinkFile := liveForkTestCreateFile(t, parentTask, "hardlink-source")
	liveForkTestPWrite(t, parentTask, hardlinkFile, 0, "seed")
	hardlinkFile.DecRef(parentTask)
	liveForkTestLink(t, parentTask, "hardlink-source", "hardlink-alias")
	truncateFile := liveForkTestCreateFile(t, parentTask, "truncate-state")
	liveForkTestPWrite(t, parentTask, truncateFile, 0, "seed")
	const truncateFD = int32(21)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, truncateFD, truncateFile, kernel.FDFlags{}); err != nil {
		truncateFile.DecRef(parentTask)
		t.Fatalf("installing truncate file: %v", err)
	}
	truncateFile.DecRef(parentTask)
	parentTruncateFile := liveForkTestFD(t, l.k, truncateFD)
	devMountFile := liveForkTestCreateFile(t, parentTask, "dev/shm/ttc-mount-state")
	const devMountFD = int32(26)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, devMountFD, devMountFile, kernel.FDFlags{}); err != nil {
		devMountFile.DecRef(parentTask)
		t.Fatalf("installing /dev/shm tmpfs file: %v", err)
	}
	devMountFile.DecRef(parentTask)
	parentDevMountFile := liveForkTestFD(t, l.k, devMountFD)
	liveForkTestPWrite(t, parentTask, parentDevMountFile, 0, "seed")
	tmpfsHardlink := liveForkTestCreateFile(t, parentTask, "dev/shm/tmpfs-hardlink-source")
	liveForkTestPWrite(t, parentTask, tmpfsHardlink, 0, "seed")
	tmpfsHardlink.DecRef(parentTask)
	liveForkTestLink(t, parentTask, "dev/shm/tmpfs-hardlink-source", "dev/shm/tmpfs-hardlink-alias")
	tmpfsUnlinked := liveForkTestCreateFile(t, parentTask, "dev/shm/tmpfs-unlinked")
	liveForkTestPWrite(t, parentTask, tmpfsUnlinked, 0, "seed")
	const tmpfsUnlinkedFD = int32(31)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, tmpfsUnlinkedFD, tmpfsUnlinked, kernel.FDFlags{}); err != nil {
		tmpfsUnlinked.DecRef(parentTask)
		t.Fatalf("installing open-unlinked tmpfs file: %v", err)
	}
	tmpfsUnlinked.DecRef(parentTask)
	liveForkTestUnlink(t, parentTask, "dev/shm/tmpfs-unlinked")
	parentTmpfsUnlinked := liveForkTestFD(t, l.k, tmpfsUnlinkedFD)
	tmpfsTruncate := liveForkTestCreateFile(t, parentTask, "dev/shm/tmpfs-truncate")
	liveForkTestPWrite(t, parentTask, tmpfsTruncate, 0, "seed")
	const tmpfsTruncateFD = int32(32)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, tmpfsTruncateFD, tmpfsTruncate, kernel.FDFlags{}); err != nil {
		tmpfsTruncate.DecRef(parentTask)
		t.Fatalf("installing truncate tmpfs file: %v", err)
	}
	tmpfsTruncate.DecRef(parentTask)
	parentTmpfsTruncate := liveForkTestFD(t, l.k, tmpfsTruncateFD)
	managedVolumeFile := liveForkTestCreateFile(t, parentTask, "managed/state")
	const managedVolumeFD = int32(28)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, managedVolumeFD, managedVolumeFile, kernel.FDFlags{}); err != nil {
		managedVolumeFile.DecRef(parentTask)
		t.Fatalf("installing managed-volume file: %v", err)
	}
	managedVolumeFile.DecRef(parentTask)
	parentManagedVolume := liveForkTestFD(t, l.k, managedVolumeFD)
	liveForkTestPWrite(t, parentTask, parentManagedVolume, 0, "seed")
	parentManagedAlias := liveForkTestOpenPath(t, parentTask, "managed-alias/state", linux.O_RDWR)
	liveForkTestPRead(t, parentTask, parentManagedAlias, 0, "seed")
	const internalConsoleID = "init"
	const consoleBaseFD = int32(0)
	parentConsoleStdin := liveForkTestFD(t, l.k, consoleBaseFD)
	parentConsoleStdout := liveForkTestFD(t, l.k, consoleBaseFD+1)
	parentConsoleStderr := liveForkTestFD(t, l.k, consoleBaseFD+2)
	liveForkTestConsoleWriteInput(t, l.k, ctx, internalConsoleID, "seed-input")
	liveForkTestRead(t, parentTask, parentConsoleStdin, "seed-input")
	liveForkTestWrite(t, parentTask, parentConsoleStderr, "seed-error")
	liveForkTestConsoleRead(t, l.k, ctx, internalConsoleID, kernel.InternalConsoleStderr, "seed-error")
	liveForkTestConsoleWriteInput(t, l.k, ctx, internalConsoleID, "fork-input")
	liveForkTestWrite(t, parentTask, parentConsoleStdout, "fork-output")
	ptyMaster := liveForkTestOpenPath(t, parentTask, "dev/ptmx", linux.O_RDWR|linux.O_NONBLOCK)
	const ptyMasterFD = int32(27)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, ptyMasterFD, ptyMaster, kernel.FDFlags{}); err != nil {
		ptyMaster.DecRef(parentTask)
		t.Fatalf("installing internal PTY master: %v", err)
	}
	ptyMaster.DecRef(parentTask)
	parentPTYMaster := liveForkTestFD(t, l.k, ptyMasterFD)

	// Keep a regular file, directory cursor, and executable mapping backed by
	// the read-only host bind alive across every fork boundary.
	const readonlyFileFD = int32(29)
	readonlyFile := liveForkTestOpenPath(t, parentTask, "readonly/fixture", linux.O_RDONLY)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, readonlyFileFD, readonlyFile, kernel.FDFlags{}); err != nil {
		readonlyFile.DecRef(parentTask)
		t.Fatalf("installing read-only bind file: %v", err)
	}
	readonlyFile.DecRef(parentTask)
	parentReadonlyFile := liveForkTestFD(t, l.k, readonlyFileFD)
	liveForkTestRead(t, ctx, parentReadonlyFile, "ho")

	const readonlyDirFD = int32(30)
	readonlyDir := liveForkTestOpenPath(t, parentTask, "readonly", linux.O_RDONLY|linux.O_DIRECTORY)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, readonlyDirFD, readonlyDir, kernel.FDFlags{}); err != nil {
		readonlyDir.DecRef(parentTask)
		t.Fatalf("installing read-only bind directory: %v", err)
	}
	readonlyDir.DecRef(parentTask)
	parentReadonlyDir := liveForkTestFD(t, l.k, readonlyDirFD)
	if got := liveForkTestNextDirent(t, ctx, parentReadonlyDir); got != "." {
		t.Fatalf("first read-only bind dirent: got %q, want .", got)
	}

	readonlyMapOpts := memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Perms:    hostarch.ReadExecute,
		MaxPerms: hostarch.ReadExecute,
		Private:  true,
	}
	if err := parentReadonlyFile.ConfigureMMap(ctx, &readonlyMapOpts); err != nil {
		t.Fatalf("configuring read-only executable mapping: %v", err)
	}
	readonlyMapAddr, err := parentTask.MemoryManager().MMap(ctx, readonlyMapOpts)
	readonlyMapOpts.MappingIdentity.DecRef(ctx)
	if err != nil {
		t.Fatalf("mapping read-only executable: %v", err)
	}
	liveForkTestMMReadContext(t, ctx, parentTask, readonlyMapAddr, "host-data")

	// Use the batch path so the primary child is serialized once more to seed
	// its sibling. This catches missing save/resume lifecycle pairing on the
	// primary (in particular, TCP beforeSave freezes endpoint packet queues).
	childInfos, err := l.family.ForkMany(&ForkBranchesArgs{IDs: []string{"child", "batch-sibling"}})
	parentTCPClient.EventUnregister(&transientNetworkWaiter)
	if err != nil {
		t.Fatalf("forking live loader: %v", err)
	}
	childInfo := childInfos[0]
	if got, want := childInfo.State, BranchRunning; got != want {
		t.Errorf("child state after fork: got %q, want %q", got, want)
	}
	child, err := l.family.branch(childInfo.ID)
	if err != nil {
		t.Fatalf("looking up child: %v", err)
	}
	if l.k.RootNetworkNamespace().Stack() == child.k.RootNetworkNamespace().Stack() {
		t.Error("parent and child share a network stack object")
	}
	parentPID := l.k.RootPIDNamespace().IDOfThreadGroup(l.k.GlobalInit())
	childPID := child.k.RootPIDNamespace().IDOfThreadGroup(child.k.GlobalInit())
	if parentPID != childPID {
		t.Errorf("init PID changed: parent=%d child=%d", parentPID, childPID)
	}
	childCtx := child.k.SupervisorContext()
	childReader := liveForkTestFD(t, child.k, pipeFDs[0])
	childWriter := liveForkTestFD(t, child.k, pipeFDs[1])
	childEvent := liveForkTestFD(t, child.k, eventFD)
	childEpollFile := liveForkTestFD(t, child.k, epollFD)
	childEpoll := childEpollFile.Impl().(*vfs.EpollInstance)
	childTimerFile := liveForkTestFD(t, child.k, timerFD)
	childTimer := childTimerFile.Impl().(*timerfd.TimerFileDescription)
	childTask := child.k.GlobalInit().Leader()
	liveForkTestMMRead(t, childTask, shmAddr1, "seed")
	liveForkTestMMRead(t, childTask, shmAddr2, "seed")
	liveForkTestMMWrite(t, childTask, shmAddr1, "chld")
	liveForkTestMMRead(t, childTask, shmAddr2, "chld")
	liveForkTestMMRead(t, parentTask, shmAddr1, "seed")
	liveForkTestMMWrite(t, parentTask, shmAddr2, "prnt")
	liveForkTestMMRead(t, parentTask, shmAddr1, "prnt")
	liveForkTestMMRead(t, childTask, shmAddr2, "chld")
	childSignalFile := liveForkTestFD(t, child.k, signalFD)
	childRoot := childTask.MountNamespace().Root(childCtx)
	childInotifyFile := liveForkTestFD(t, child.k, inotifyFD)
	childInotify := childInotifyFile.Impl().(*vfs.Inotify)
	childMemfd := liveForkTestFD(t, child.k, memfdFD)
	childOverlayFile := liveForkTestFD(t, child.k, overlayFD)
	childTruncateFile := liveForkTestFD(t, child.k, truncateFD)
	childDevMountFile := liveForkTestFD(t, child.k, devMountFD)
	childTmpfsUnlinked := liveForkTestFD(t, child.k, tmpfsUnlinkedFD)
	childTmpfsTruncate := liveForkTestFD(t, child.k, tmpfsTruncateFD)
	childManagedVolume := liveForkTestFD(t, child.k, managedVolumeFD)
	childManagedAlias := liveForkTestOpenPath(t, childTask, "managed-alias/state", linux.O_RDWR)
	childConsoleStdin := liveForkTestFD(t, child.k, consoleBaseFD)
	childConsoleStdout := liveForkTestFD(t, child.k, consoleBaseFD+1)
	childPTYMaster := liveForkTestFD(t, child.k, ptyMasterFD)
	childReadonlyFile := liveForkTestFD(t, child.k, readonlyFileFD)
	childReadonlyDir := liveForkTestFD(t, child.k, readonlyDirFD)
	liveForkTestMMReadContext(t, childCtx, childTask, readonlyMapAddr, "host-data")
	if parentPTYMaster.Impl() == childPTYMaster.Impl() {
		t.Fatal("parent and child share the same internal PTY master object")
	}
	for _, check := range []struct {
		task *kernel.Task
		path string
	}{
		{parentTask, "sys"},
		{childTask, "sys"},
	} {
		file := liveForkTestOpenPath(t, check.task, check.path, linux.O_RDONLY)
		file.DecRef(check.task)
	}

	// Every dynamic procfs view must resolve through the selected branch Kernel.
	// Use the task itself as context so /proc/self is not accidentally resolved
	// against the supervisor.
	procStatuses := map[string]string{
		"parent-self": liveForkTestReadAllPathContext(t, parentTask, parentTask, "proc/self/status"),
		"parent-pid1": liveForkTestReadAllPathContext(t, parentTask, parentTask, "proc/1/status"),
		"child-self":  liveForkTestReadAllPathContext(t, childTask, childTask, "proc/self/status"),
		"child-pid1":  liveForkTestReadAllPathContext(t, childTask, childTask, "proc/1/status"),
	}
	for name, status := range procStatuses {
		for _, field := range []string{"Pid:\t1\n", "PPid:\t0\n"} {
			if !strings.Contains(status, field) {
				t.Errorf("%s status omitted %q: %q", name, field, status)
			}
		}
	}
	parentMountInfo := liveForkTestReadAllPathContext(t, parentTask, parentTask, "proc/self/mountinfo")
	childMountInfo := liveForkTestReadAllPathContext(t, childTask, childTask, "proc/self/mountinfo")
	if diff := cmp.Diff(parentMountInfo, childMountInfo); diff != "" {
		t.Errorf("live fork changed /proc/self/mountinfo (-parent +child):\n%s", diff)
	}
	for _, mountPath := range []string{"/proc", "/dev/shm", "/sys", "/readonly", "/managed", "/managed-alias"} {
		if !strings.Contains(parentMountInfo, " "+mountPath+" ") {
			t.Errorf("/proc/self/mountinfo omitted %q: %q", mountPath, parentMountInfo)
		}
	}
	var parentNetDev string
	normalizeProcNet := func(name, contents string) string {
		switch name {
		case "tcp", "tcp6", "udp", "udp6", "unix":
			lines := strings.Split(strings.TrimSuffix(contents, "\n"), "\n")
			sort.Strings(lines)
			return strings.Join(lines, "\n") + "\n"
		default:
			return contents
		}
	}
	for _, name := range []string{"dev", "snmp", "route", "tcp", "tcp6", "udp", "udp6", "unix"} {
		path := "proc/net/" + name
		parentContents := liveForkTestReadAllPathContext(t, parentTask, parentTask, path)
		childContents := liveForkTestReadAllPathContext(t, childTask, childTask, path)
		if diff := cmp.Diff(normalizeProcNet(name, parentContents), normalizeProcNet(name, childContents)); diff != "" {
			t.Errorf("live fork changed /%s (-parent +child):\n%s", path, diff)
		}
		if name == "dev" {
			parentNetDev = parentContents
		}
	}
	if !strings.Contains(parentNetDev, "lo:") {
		t.Errorf("/proc/net/dev omitted contained loopback: %q", parentNetDev)
	}
	var parentUptime, childUptime float64
	if _, err := fmt.Sscanf(liveForkTestReadAllPathContext(t, parentTask, parentTask, "proc/uptime"), "%f", &parentUptime); err != nil {
		t.Fatalf("parsing parent /proc/uptime: %v", err)
	}
	if _, err := fmt.Sscanf(liveForkTestReadAllPathContext(t, childTask, childTask, "proc/uptime"), "%f", &childUptime); err != nil {
		t.Fatalf("parsing child /proc/uptime: %v", err)
	}
	if delta := parentUptime - childUptime; delta < -1 || delta > 1 {
		t.Errorf("live fork changed procfs uptime discontinuously: parent=%f child=%f", parentUptime, childUptime)
	}
	parentSelfPIDNS := liveForkTestOpenPathContext(t, parentTask, parentTask, "proc/self/ns/pid", linux.O_RDONLY)
	parentPID1NS := liveForkTestOpenPathContext(t, parentTask, parentTask, "proc/1/ns/pid", linux.O_RDONLY)
	childSelfPIDNS := liveForkTestOpenPathContext(t, childTask, childTask, "proc/self/ns/pid", linux.O_RDONLY)
	childPID1NS := liveForkTestOpenPathContext(t, childTask, childTask, "proc/1/ns/pid", linux.O_RDONLY)
	namespaceInode := func(name string, task *kernel.Task, file *vfs.FileDescription) uint64 {
		stat, err := file.Stat(task, vfs.StatOptions{Mask: linux.STATX_INO})
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		return stat.Ino
	}
	parentSelfPIDNSIno := namespaceInode("parent /proc/self/ns/pid", parentTask, parentSelfPIDNS)
	parentPID1NSIno := namespaceInode("parent /proc/1/ns/pid", parentTask, parentPID1NS)
	childSelfPIDNSIno := namespaceInode("child /proc/self/ns/pid", childTask, childSelfPIDNS)
	childPID1NSIno := namespaceInode("child /proc/1/ns/pid", childTask, childPID1NS)
	if parentSelfPIDNSIno != parentPID1NSIno {
		t.Errorf("parent /proc/self/ns/pid inode %d differs from PID 1 namespace inode %d", parentSelfPIDNSIno, parentPID1NSIno)
	}
	if childSelfPIDNSIno != childPID1NSIno {
		t.Errorf("child /proc/self/ns/pid inode %d differs from PID 1 namespace inode %d", childSelfPIDNSIno, childPID1NSIno)
	}
	if parentSelfPIDNSIno != childSelfPIDNSIno {
		t.Errorf("live fork changed observable PID namespace inode: parent=%d child=%d", parentSelfPIDNSIno, childSelfPIDNSIno)
	}
	if parentSelfPIDNS.VirtualDentry().Dentry() == childSelfPIDNS.VirtualDentry().Dentry() {
		t.Error("parent and child procfs namespace handles share a source-Kernel dentry")
	}
	parentSelfPIDNS.DecRef(parentTask)
	parentPID1NS.DecRef(parentTask)
	childSelfPIDNS.DecRef(childTask)
	childPID1NS.DecRef(childTask)

	// Acquire matching branch-local test references only after procfs has been
	// observed, so the proc socket refcounts are compared symmetrically.
	childUnix1 := liveForkTestFD(t, child.k, unixFDs[0])
	childUnix2 := liveForkTestFD(t, child.k, unixFDs[1])
	childUDPClient := liveForkTestFD(t, child.k, udpFDs[0])
	childUDPServer := liveForkTestFD(t, child.k, udpFDs[1])
	childTCPClient := liveForkTestFD(t, child.k, tcpFDs[0])
	childTCPServer := liveForkTestFD(t, child.k, tcpFDs[1])
	childNetlink := liveForkTestFD(t, child.k, netlinkFD)
	if parentNetlink.Impl() == childNetlink.Impl() {
		t.Fatal("parent and child share a netlink socket object")
	}

	liveForkTestPathRead(t, parentTask, "readonly/fixture", "host-data")
	liveForkTestPathRead(t, childTask, "readonly/fixture", "host-data")
	liveForkTestPathWriteDenied(t, parentTask, "readonly/fixture")
	liveForkTestPathWriteDenied(t, childTask, "readonly/fixture")
	if got := liveForkTestNextDirent(t, ctx, parentReadonlyDir); got != ".." {
		t.Fatalf("parent read-only bind second dirent: got %q, want ..", got)
	}
	if got := liveForkTestNextDirent(t, childCtx, childReadonlyDir); got != ".." {
		t.Fatalf("child read-only bind second dirent: got %q, want ..", got)
	}
	parentDirent := liveForkTestNextDirent(t, ctx, parentReadonlyDir)
	childDirent := liveForkTestNextDirent(t, childCtx, childReadonlyDir)
	if parentDirent != childDirent {
		t.Fatalf("read-only bind cursors diverged after fork: parent=%q child=%q", parentDirent, childDirent)
	}
	if err := os.WriteFile(filepath.Join(hostBind, "fixture"), []byte("hoMUTATED"), 0644); err != nil {
		t.Fatalf("mutating read-only bind fixture on host: %v", err)
	}
	liveForkTestMMReadContext(t, ctx, parentTask, readonlyMapAddr, "hoMUTATED")
	liveForkTestMMReadContext(t, childCtx, childTask, readonlyMapAddr, "hoMUTATED")
	liveForkTestRead(t, ctx, parentReadonlyFile, "MU")
	liveForkTestRead(t, childCtx, childReadonlyFile, "MU")
	liveForkTestRead(t, ctx, parentReadonlyFile, "TA")
	liveForkTestRead(t, childCtx, childReadonlyFile, "TA")

	liveForkTestPRead(t, childTask, childManagedVolume, 0, "seed")
	liveForkTestPRead(t, childTask, childManagedAlias, 0, "seed")
	liveForkTestPWrite(t, childTask, childManagedAlias, 0, "chld")
	liveForkTestPRead(t, childTask, childManagedVolume, 0, "chld")
	liveForkTestPRead(t, parentTask, parentManagedVolume, 0, "seed")
	liveForkTestPWrite(t, parentTask, parentManagedVolume, 0, "prnt")
	liveForkTestPRead(t, parentTask, parentManagedAlias, 0, "prnt")
	liveForkTestPRead(t, childTask, childManagedVolume, 0, "chld")
	liveForkTestRead(t, parentTask, parentConsoleStdin, "fork-input")
	liveForkTestRead(t, childTask, childConsoleStdin, "fork-input")
	liveForkTestConsoleRead(t, l.k, ctx, internalConsoleID, kernel.InternalConsoleStdout, "fork-output")
	liveForkTestConsoleRead(t, child.k, childCtx, internalConsoleID, kernel.InternalConsoleStdout, "fork-output")
	liveForkTestWrite(t, childTask, childConsoleStdout, "child-output")
	liveForkTestConsoleRead(t, child.k, childCtx, internalConsoleID, kernel.InternalConsoleStdout, "child-output")
	liveForkTestConsoleWouldBlock(t, l.k, ctx, internalConsoleID, kernel.InternalConsoleStdout)
	liveForkTestWrite(t, parentTask, parentConsoleStdout, "parent-output")
	liveForkTestConsoleRead(t, l.k, ctx, internalConsoleID, kernel.InternalConsoleStdout, "parent-output")
	liveForkTestConsoleWouldBlock(t, child.k, childCtx, internalConsoleID, kernel.InternalConsoleStdout)
	liveForkTestConsoleWriteInput(t, child.k, childCtx, internalConsoleID, "child-input")
	liveForkTestRead(t, childTask, childConsoleStdin, "child-input")
	// Pipe endpoints still alias one queue inside a branch, but queues and
	// readiness are duplicated across branches.
	liveForkTestRead(t, childCtx, childReader, "seed")
	if got := childReader.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("child still readable after consuming seed: %#x", got)
	}
	if got := parentReader.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child read consumed parent's seed: parent readiness %#x", got)
	}
	liveForkEpollEvents(t, childEpoll, nil)
	liveForkEpollEvents(t, parentEpoll, []linux.EpollEvent{{
		Events: linux.EPOLLIN,
		Data:   [2]int32{epollData, 0},
	}})
	liveForkTestRead(t, ctx, parentReader, "seed")
	liveForkEpollEvents(t, parentEpoll, nil)
	liveForkTestWrite(t, ctx, parentWriter, "parent")
	if got := childReader.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent write made child readable: %#x", got)
	}
	liveForkEpollEvents(t, parentEpoll, []linux.EpollEvent{{
		Events: linux.EPOLLIN,
		Data:   [2]int32{epollData, 0},
	}})
	liveForkEpollEvents(t, childEpoll, nil)
	if err := childEpoll.DeleteInterest(childReader, pipeFDs[0]); err != nil {
		t.Fatalf("deleting child epoll interest: %v", err)
	}
	liveForkTestWrite(t, childCtx, childWriter, "child")
	liveForkEpollEvents(t, childEpoll, nil)
	liveForkTestRead(t, ctx, parentReader, "parent")
	liveForkTestRead(t, childCtx, childReader, "child")
	liveForkTestWouldBlock(t, ctx, parentReader)
	liveForkTestWouldBlock(t, childCtx, childReader)

	// Internal eventfd counters and readiness follow the same branch-local
	// semantics as pipe queues.
	liveForkEventRead(t, childCtx, childEvent, 7)
	if got := parentEvent.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child eventfd read consumed parent counter: %#x", got)
	}
	liveForkEventRead(t, ctx, parentEvent, 7)
	liveForkEventWrite(t, ctx, parentEvent, 3)
	if got := childEvent.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent eventfd write made child readable: %#x", got)
	}
	liveForkEventWrite(t, childCtx, childEvent, 4)
	liveForkEventRead(t, ctx, parentEvent, 3)
	liveForkEventRead(t, childCtx, childEvent, 4)
	liveForkEventWouldBlock(t, ctx, parentEvent)
	liveForkEventWouldBlock(t, childCtx, childEvent)

	// Timerfd expiration counters and notifications are branch-local, while
	// Resume above reconstructs each cloned timer's runtime-only host fields.
	liveForkCounterRead(t, childCtx, childTimerFile, 2, "child timerfd")
	if got := parentTimerFile.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child timerfd read consumed parent expirations: %#x", got)
	}
	liveForkCounterRead(t, ctx, parentTimerFile, 2, "parent timerfd")
	parentTimer.NotifyTimer(3)
	if got := childTimerFile.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent timerfd notification made child readable: %#x", got)
	}
	childTimer.NotifyTimer(4)
	liveForkCounterRead(t, ctx, parentTimerFile, 3, "parent timerfd")
	liveForkCounterRead(t, childCtx, childTimerFile, 4, "child timerfd")
	liveForkEventWouldBlock(t, ctx, parentTimerFile)
	liveForkEventWouldBlock(t, childCtx, childTimerFile)
	entry, timerReady := waiter.NewChannelEntry(waiter.EventIn)
	if err := childTimerFile.EventRegister(&entry); err != nil {
		t.Fatalf("registering child timerfd waiter: %v", err)
	}
	now, _ := childTimer.GetTime()
	childTimer.SetTime(ktime.Setting{Enabled: true, Next: now.Add(20 * time.Millisecond)})
	select {
	case <-timerReady:
	case <-time.After(2 * time.Second):
		t.Fatal("cloned child timerfd did not expire")
	}
	childTimerFile.EventUnregister(&entry)
	liveForkCounterRead(t, childCtx, childTimerFile, 1, "armed child timerfd")
	if got := parentTimerFile.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("child timerfd expiration made parent readable: %#x", got)
	}

	// Signalfd target-task pointers, pending queues, and waiter registrations
	// are reconstructed within each branch.
	liveForkSignalRead(t, childCtx, childSignalFile, linux.SIGUSR1)
	if got := parentSignalFile.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child signalfd read consumed parent pending signal: %#x", got)
	}
	liveForkSignalRead(t, ctx, parentSignalFile, linux.SIGUSR1)
	if err := parentTask.SendSignal(kernel.SignalInfoPriv(linux.SIGUSR1)); err != nil {
		t.Fatalf("queuing parent-only signal: %v", err)
	}
	if got := childSignalFile.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent signal made child signalfd readable: %#x", got)
	}
	if err := childTask.SendSignal(kernel.SignalInfoPriv(linux.SIGUSR1)); err != nil {
		t.Fatalf("queuing child-only signal: %v", err)
	}
	liveForkSignalRead(t, ctx, parentSignalFile, linux.SIGUSR1)
	liveForkSignalRead(t, childCtx, childSignalFile, linux.SIGUSR1)
	liveForkSignalWouldBlock(t, ctx, parentSignalFile)
	liveForkSignalWouldBlock(t, childCtx, childSignalFile)

	// Inotify watch targets, queued events, and watch-list mutations are local
	// to each cloned VFS graph.
	liveForkInotifyRead(t, childCtx, childInotifyFile, watchID, linux.IN_ATTRIB)
	if got := parentInotifyFile.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child inotify read consumed parent event: %#x", got)
	}
	liveForkInotifyRead(t, ctx, parentInotifyFile, watchID, linux.IN_ATTRIB)
	parentRoot.Dentry().Watches().Notify(ctx, "", linux.IN_ATTRIB, 0, vfs.InodeEvent, false)
	if got := childInotifyFile.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent inotify event made child readable: %#x", got)
	}
	childRoot.Dentry().Watches().Notify(childCtx, "", linux.IN_ATTRIB, 0, vfs.InodeEvent, false)
	liveForkInotifyRead(t, ctx, parentInotifyFile, watchID, linux.IN_ATTRIB)
	liveForkInotifyRead(t, childCtx, childInotifyFile, watchID, linux.IN_ATTRIB)
	if err := childInotify.RmWatch(childCtx, watchID); err != nil {
		t.Fatalf("removing child inotify watch: %v", err)
	}
	liveForkInotifyRead(t, childCtx, childInotifyFile, watchID, linux.IN_IGNORED)
	parentRoot.Dentry().Watches().Notify(ctx, "", linux.IN_ATTRIB, 0, vfs.InodeEvent, false)
	liveForkInotifyRead(t, ctx, parentInotifyFile, watchID, linux.IN_ATTRIB)
	liveForkInotifyWouldBlock(t, childCtx, childInotifyFile)

	// A connected internal Unix socketpair preserves peer topology inside each
	// branch, but receive queues and readiness are duplicated at the fork.
	liveForkTestRead(t, childTask, childUnix2, "seed")
	if got := parentUnix2.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child Unix read consumed parent queued packet: %#x", got)
	}
	liveForkTestRead(t, parentTask, parentUnix2, "seed")
	liveForkTestWrite(t, parentTask, parentUnix1, "parent")
	if got := childUnix2.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent Unix write made child peer readable: %#x", got)
	}
	liveForkTestWrite(t, childTask, childUnix1, "child")
	liveForkTestRead(t, parentTask, parentUnix2, "parent")
	liveForkTestRead(t, childTask, childUnix2, "child")
	liveForkTestWouldBlock(t, parentTask, parentUnix2)
	liveForkTestWouldBlock(t, childTask, childUnix2)

	// The unread loopback datagram is duplicated at the fork. Subsequent sends
	// are routed only through the branch-local cloned stack and endpoint graph.
	liveForkTestRead(t, parentTask, parentUDPServer, "seed")
	liveForkTestRead(t, childTask, childUDPServer, "seed")
	liveForkTestWrite(t, parentTask, parentUDPClient, "parn")
	liveForkTestRead(t, parentTask, parentUDPServer, "parn")
	liveForkTestWouldBlock(t, childTask, childUDPServer)
	liveForkTestWrite(t, childTask, childUDPClient, "chld")
	liveForkTestRead(t, childTask, childUDPServer, "chld")
	liveForkTestWouldBlock(t, parentTask, parentUDPServer)

	// Established loopback TCP stream state and unread receive data follow the
	// same branch-local semantics as the UDP endpoint graph.
	liveForkTestRead(t, parentTask, parentTCPServer, "seed")
	liveForkTestRead(t, childTask, childTCPServer, "seed")
	liveForkTestWrite(t, parentTask, parentTCPClient, "parn")
	liveForkTestReadEventually(t, parentTask, parentTCPServer, "parn")
	liveForkTestWouldBlock(t, childTask, childTCPServer)
	liveForkTestWrite(t, childTask, childTCPClient, "chld")
	liveForkTestReadEventually(t, childTask, childTCPServer, "chld")
	liveForkTestWouldBlock(t, parentTask, parentTCPServer)

	// Tmpfs syscall writes and mapped shared writes detach MemoryFile pages
	// without copying file contents at the fork boundary.
	liveForkTestPRead(t, parentTask, parentMemfd, 0, "seed")
	liveForkTestPRead(t, childTask, childMemfd, 0, "seed")
	liveForkTestPWrite(t, parentTask, parentMemfd, 0, "prnt")
	liveForkTestPRead(t, parentTask, parentMemfd, 0, "prnt")
	liveForkTestPRead(t, childTask, childMemfd, 0, "seed")
	liveForkTestMappedWrite(t, childTask, childMemfd, "chld")
	liveForkTestPRead(t, childTask, childMemfd, 0, "chld")
	liveForkTestPRead(t, parentTask, parentMemfd, 0, "prnt")
	parentProcMemfd := liveForkTestOpenPathContext(t, parentTask, parentTask, "proc/self/fd/19", linux.O_RDONLY)
	childProcMemfd := liveForkTestOpenPathContext(t, childTask, childTask, "proc/self/fd/19", linux.O_RDONLY)
	liveForkTestPRead(t, parentTask, parentProcMemfd, 0, "prnt")
	liveForkTestPRead(t, childTask, childProcMemfd, 0, "chld")
	parentProcMemfd.DecRef(parentTask)
	childProcMemfd.DecRef(childTask)

	// An explicit tmpfs submount uses the Kernel MemoryFile and therefore
	// detaches syscall and mapped writes without eager copying. Its inode graph,
	// metadata, unlinked open files, allocation state, and mount flags are also
	// branch-local.
	wantTmpfsMountFlags := uint64(linux.ST_NOSUID | linux.ST_NOEXEC | linux.ST_NODEV)
	for name, file := range map[string]*vfs.FileDescription{"parent": parentDevMountFile, "child": childDevMountFile} {
		if got := file.Mount().MountFlags(); got != wantTmpfsMountFlags {
			t.Errorf("%s /dev/shm mount flags: got %#x, want %#x", name, got, wantTmpfsMountFlags)
		}
	}
	liveForkTestPRead(t, parentTask, parentDevMountFile, 0, "seed")
	liveForkTestPRead(t, childTask, childDevMountFile, 0, "seed")
	liveForkTestPWrite(t, parentTask, parentDevMountFile, 0, "prnt")
	liveForkTestPRead(t, childTask, childDevMountFile, 0, "seed")
	liveForkTestMappedWrite(t, childTask, childDevMountFile, "chld")
	liveForkTestPRead(t, parentTask, parentDevMountFile, 0, "prnt")
	liveForkTestPRead(t, childTask, childDevMountFile, 0, "chld")
	if err := parentDevMountFile.SetStat(ctx, vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_MODE, Mode: uint16(linux.ModeRegular | 0600)}}); err != nil {
		t.Fatalf("changing parent tmpfs mode: %v", err)
	}
	liveForkTestMode(t, ctx, parentDevMountFile, 0600)
	liveForkTestMode(t, childCtx, childDevMountFile, 0644)

	parentTmpfsHardlink := liveForkTestOpenPath(t, parentTask, "dev/shm/tmpfs-hardlink-alias", linux.O_RDWR)
	childTmpfsHardlink := liveForkTestOpenPath(t, childTask, "dev/shm/tmpfs-hardlink-alias", linux.O_RDWR)
	liveForkTestPWrite(t, parentTask, parentTmpfsHardlink, 0, "PARN")
	liveForkTestPathRead(t, parentTask, "dev/shm/tmpfs-hardlink-source", "PARN")
	liveForkTestPRead(t, childTask, childTmpfsHardlink, 0, "seed")
	liveForkTestUnlink(t, childTask, "dev/shm/tmpfs-hardlink-alias")
	liveForkTestPathMissing(t, childTask, "dev/shm/tmpfs-hardlink-alias")
	liveForkTestPathRead(t, childTask, "dev/shm/tmpfs-hardlink-source", "seed")
	liveForkTestPathRead(t, parentTask, "dev/shm/tmpfs-hardlink-alias", "PARN")
	parentTmpfsHardlink.DecRef(ctx)
	childTmpfsHardlink.DecRef(childCtx)

	liveForkTestPRead(t, parentTask, parentTmpfsUnlinked, 0, "seed")
	liveForkTestPRead(t, childTask, childTmpfsUnlinked, 0, "seed")
	liveForkTestPWrite(t, parentTask, parentTmpfsUnlinked, 0, "PUNL")
	liveForkTestMappedWrite(t, childTask, childTmpfsUnlinked, "CUNL")
	liveForkTestPRead(t, parentTask, parentTmpfsUnlinked, 0, "PUNL")
	liveForkTestPRead(t, childTask, childTmpfsUnlinked, 0, "CUNL")
	liveForkTestPathMissing(t, parentTask, "dev/shm/tmpfs-unlinked")
	liveForkTestPathMissing(t, childTask, "dev/shm/tmpfs-unlinked")

	if err := childTmpfsTruncate.SetStat(childCtx, vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_SIZE, Size: 2}}); err != nil {
		t.Fatalf("truncating child tmpfs file: %v", err)
	}
	liveForkTestPRead(t, parentTask, parentTmpfsTruncate, 0, "seed")
	liveForkTestPRead(t, childTask, childTmpfsTruncate, 0, "se")
	liveForkTestSize(t, ctx, parentTmpfsTruncate, 4)
	liveForkTestSize(t, childCtx, childTmpfsTruncate, 2)
	if err := parentTmpfsTruncate.Allocate(ctx, 0, hostarch.PageSize, hostarch.PageSize); err != nil {
		t.Fatalf("allocating sparse parent tmpfs range: %v", err)
	}
	liveForkTestSize(t, ctx, parentTmpfsTruncate, 2*hostarch.PageSize)
	liveForkTestSize(t, childCtx, childTmpfsTruncate, 2)

	// The writable root uses a branch-local tmpfs OverlayFS upper over the
	// shared immutable gofer lower. Contents, open-unlinked files, and directory
	// topology diverge independently.
	liveForkTestPRead(t, parentTask, parentOverlayFile, 0, "seed")
	liveForkTestPRead(t, childTask, childOverlayFile, 0, "seed")
	liveForkTestPWrite(t, parentTask, parentOverlayFile, 0, "prnt")
	liveForkTestMappedWrite(t, childTask, childOverlayFile, "chld")
	liveForkTestPRead(t, parentTask, parentOverlayFile, 0, "prnt")
	liveForkTestPRead(t, childTask, childOverlayFile, 0, "chld")
	if err := parentOverlayFile.SetStat(ctx, vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_MODE, Mode: uint16(linux.ModeRegular | 0600)}}); err != nil {
		t.Fatalf("changing parent overlay mode: %v", err)
	}
	liveForkTestMode(t, ctx, parentOverlayFile, 0600)
	liveForkTestMode(t, childCtx, childOverlayFile, 0644)
	liveForkTestUnlink(t, childTask, overlayPath)
	liveForkTestPathMissing(t, childTask, overlayPath)
	liveForkTestPathRead(t, parentTask, overlayPath, "prnt")
	parentOnly := liveForkTestCreateFile(t, parentTask, "parent-only")
	parentOnly.DecRef(parentTask)
	liveForkTestPathMissing(t, childTask, "parent-only")
	liveForkTestPRead(t, childTask, childOverlayFile, 0, "chld")
	parentHardlink := liveForkTestOpenPath(t, parentTask, "hardlink-alias", linux.O_RDWR)
	childHardlink := liveForkTestOpenPath(t, childTask, "hardlink-alias", linux.O_RDWR)
	liveForkTestPWrite(t, ctx, parentHardlink, 0, "PARN")
	liveForkTestPathRead(t, parentTask, "hardlink-source", "PARN")
	liveForkTestPRead(t, childCtx, childHardlink, 0, "seed")
	liveForkTestUnlink(t, childTask, "hardlink-alias")
	liveForkTestPathMissing(t, childTask, "hardlink-alias")
	liveForkTestPathRead(t, childTask, "hardlink-source", "seed")
	liveForkTestPathRead(t, parentTask, "hardlink-alias", "PARN")
	parentHardlink.DecRef(ctx)
	childHardlink.DecRef(childCtx)
	if err := childTruncateFile.SetStat(childCtx, vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_SIZE, Size: 2}}); err != nil {
		t.Fatalf("truncating child upper file: %v", err)
	}
	liveForkTestPRead(t, parentTask, parentTruncateFile, 0, "seed")
	liveForkTestPRead(t, childTask, childTruncateFile, 0, "se")
	liveForkTestSize(t, ctx, parentTruncateFile, 4)
	liveForkTestSize(t, childCtx, childTruncateFile, 2)
	if err := parentTruncateFile.Allocate(ctx, 0, hostarch.PageSize, hostarch.PageSize); err != nil {
		t.Fatalf("allocating sparse parent upper range: %v", err)
	}
	liveForkTestSize(t, ctx, parentTruncateFile, 2*hostarch.PageSize)
	liveForkTestSize(t, childCtx, childTruncateFile, 2)
	liveForkTestPRead(t, childTask, childTruncateFile, 0, "se")
	parentLowerFile := liveForkTestOpenPath(t, parentTask, "etc/hostname", linux.O_RDONLY)
	originalLower := liveForkTestPReadString(t, parentTask.Kernel().SupervisorContext(), parentLowerFile, 0, 4)
	childCopiedUpFile := liveForkTestOpenPath(t, childTask, "etc/hostname", linux.O_RDWR)
	liveForkTestPWrite(t, childCtx, childCopiedUpFile, 0, "CHLD")
	liveForkTestPRead(t, childCtx, childCopiedUpFile, 0, "CHLD")
	liveForkTestPRead(t, ctx, parentLowerFile, 0, originalLower)
	liveForkTestUnlink(t, childTask, "etc/hostname")
	liveForkTestPathMissing(t, childTask, "etc/hostname")
	liveForkTestPRead(t, childCtx, childCopiedUpFile, 0, "CHLD")
	liveForkTestPRead(t, ctx, parentLowerFile, 0, originalLower)
	parentLowerFile.DecRef(ctx)
	childCopiedUpFile.DecRef(childCtx)

	liveForkTestWrite(t, childTask, childUDPClient, "fork")
	liveForkTestWrite(t, childTask, childTCPClient, "fork")
	liveForkTestWrite(t, childTask, childConsoleStdout, "recursive-output")
	liveForkTestWaitReadable(t, childTCPServer)
	grandchildInfo, err := l.family.Fork(&ForkBranchArgs{Source: childInfo.ID, ID: "grandchild", Paused: true})
	if err != nil {
		t.Fatalf("recursively forking child: %v", err)
	}
	grandchild, err := l.family.branch(grandchildInfo.ID)
	if err != nil {
		t.Fatalf("looking up grandchild: %v", err)
	}
	grandchildTask := grandchild.k.GlobalInit().Leader()
	grandchildCtx := grandchild.k.SupervisorContext()
	grandchildUDPClient := liveForkTestFD(t, grandchild.k, udpFDs[0])
	grandchildUDPServer := liveForkTestFD(t, grandchild.k, udpFDs[1])
	grandchildTCPClient := liveForkTestFD(t, grandchild.k, tcpFDs[0])
	grandchildTCPServer := liveForkTestFD(t, grandchild.k, tcpFDs[1])
	liveForkTestRead(t, childTask, childUDPServer, "fork")
	liveForkTestRead(t, grandchildTask, grandchildUDPServer, "fork")
	liveForkTestWrite(t, grandchildTask, grandchildUDPClient, "grnd")
	liveForkTestRead(t, grandchildTask, grandchildUDPServer, "grnd")
	liveForkTestWouldBlock(t, childTask, childUDPServer)
	liveForkTestRead(t, childTask, childTCPServer, "fork")
	liveForkTestRead(t, grandchildTask, grandchildTCPServer, "fork")
	liveForkTestWrite(t, grandchildTask, grandchildTCPClient, "grnd")
	liveForkTestReadEventually(t, grandchildTask, grandchildTCPServer, "grnd")
	liveForkTestWouldBlock(t, childTask, childTCPServer)
	liveForkTestMMRead(t, grandchildTask, shmAddr1, "chld")
	liveForkTestMMWrite(t, grandchildTask, shmAddr2, "grnd")
	liveForkTestMMRead(t, grandchildTask, shmAddr1, "grnd")
	liveForkTestMMRead(t, childTask, shmAddr1, "chld")
	liveForkTestMMRead(t, parentTask, shmAddr1, "prnt")
	grandchildMemfd := liveForkTestFD(t, grandchild.k, memfdFD)
	grandchildOverlayFile := liveForkTestFD(t, grandchild.k, overlayFD)
	grandchildDevMountFile := liveForkTestFD(t, grandchild.k, devMountFD)
	grandchildTmpfsUnlinked := liveForkTestFD(t, grandchild.k, tmpfsUnlinkedFD)
	grandchildTmpfsTruncate := liveForkTestFD(t, grandchild.k, tmpfsTruncateFD)
	grandchildConsoleStdin := liveForkTestFD(t, grandchild.k, consoleBaseFD)
	grandchildConsoleStdout := liveForkTestFD(t, grandchild.k, consoleBaseFD+1)
	liveForkTestPRead(t, grandchildTask, grandchildMemfd, 0, "chld")
	liveForkTestPRead(t, grandchildTask, grandchildOverlayFile, 0, "chld")
	liveForkTestPRead(t, grandchildTask, grandchildDevMountFile, 0, "chld")
	liveForkTestPRead(t, grandchildTask, grandchildTmpfsUnlinked, 0, "CUNL")
	liveForkTestPRead(t, grandchildTask, grandchildTmpfsTruncate, 0, "se")
	if got := grandchildDevMountFile.Mount().MountFlags(); got != wantTmpfsMountFlags {
		t.Errorf("grandchild /dev/shm mount flags: got %#x, want %#x", got, wantTmpfsMountFlags)
	}
	liveForkTestPWrite(t, grandchildTask, grandchildDevMountFile, 0, "grnd")
	liveForkTestMappedWrite(t, grandchildTask, grandchildTmpfsUnlinked, "GUNL")
	liveForkTestPRead(t, childTask, childDevMountFile, 0, "chld")
	liveForkTestPRead(t, childTask, childTmpfsUnlinked, 0, "CUNL")
	grandchildManagedVolume := liveForkTestFD(t, grandchild.k, managedVolumeFD)
	grandchildManagedAlias := liveForkTestOpenPath(t, grandchildTask, "managed-alias/state", linux.O_RDWR)
	grandchildReadonlyFile := liveForkTestFD(t, grandchild.k, readonlyFileFD)
	grandchildReadonlyDir := liveForkTestFD(t, grandchild.k, readonlyDirFD)
	liveForkTestMMReadContext(t, childCtx, childTask, readonlyMapAddr, "hoMUTATED")
	liveForkTestMMReadContext(t, grandchildCtx, grandchildTask, readonlyMapAddr, "hoMUTATED")
	liveForkTestRead(t, childCtx, childReadonlyFile, "TE")
	liveForkTestRead(t, grandchildCtx, grandchildReadonlyFile, "TE")
	recursiveDirent := liveForkTestNextDirent(t, childCtx, childReadonlyDir)
	if got := liveForkTestNextDirent(t, grandchildCtx, grandchildReadonlyDir); got != recursiveDirent {
		t.Fatalf("read-only bind cursor diverged after recursive fork: child=%q grandchild=%q", recursiveDirent, got)
	}
	liveForkTestPathRead(t, grandchildTask, "readonly/fixture", "hoMUTATED")
	liveForkTestPathMissing(t, grandchildTask, overlayPath)
	liveForkTestPathMissing(t, grandchildTask, "parent-only")
	liveForkTestPRead(t, grandchildTask, grandchildManagedVolume, 0, "chld")
	liveForkTestPRead(t, grandchildTask, grandchildManagedAlias, 0, "chld")
	liveForkTestConsoleRead(t, child.k, childCtx, internalConsoleID, kernel.InternalConsoleStdout, "recursive-output")
	liveForkTestConsoleRead(t, grandchild.k, grandchildTask, internalConsoleID, kernel.InternalConsoleStdout, "recursive-output")
	liveForkTestWrite(t, grandchildTask, grandchildConsoleStdout, "grandchild-output")
	liveForkTestConsoleRead(t, grandchild.k, grandchildTask, internalConsoleID, kernel.InternalConsoleStdout, "grandchild-output")
	liveForkTestConsoleWouldBlock(t, child.k, childCtx, internalConsoleID, kernel.InternalConsoleStdout)
	liveForkTestPWrite(t, grandchildTask, grandchildMemfd, 0, "grnd")
	liveForkTestPWrite(t, grandchildTask, grandchildOverlayFile, 0, "grnd")
	liveForkTestPRead(t, childTask, childMemfd, 0, "chld")
	liveForkTestPRead(t, childTask, childOverlayFile, 0, "chld")

	liveForkTestPWrite(t, grandchildTask, grandchildManagedAlias, 0, "grnd")
	liveForkTestPRead(t, grandchildTask, grandchildManagedVolume, 0, "grnd")
	liveForkTestPRead(t, childTask, childManagedVolume, 0, "chld")
	childReader.DecRef(childCtx)
	childWriter.DecRef(childCtx)
	childEvent.DecRef(childCtx)
	childEpollFile.DecRef(childCtx)
	childTimerFile.DecRef(childCtx)
	childSignalFile.DecRef(childCtx)
	childInotifyFile.DecRef(childCtx)
	childRoot.DecRef(childCtx)
	childUnix1.DecRef(childTask)
	childUnix2.DecRef(childTask)
	childUDPClient.DecRef(childTask)
	childUDPServer.DecRef(childTask)
	childTCPClient.DecRef(childTask)
	childTCPServer.DecRef(childTask)
	childNetlink.DecRef(childTask)
	childMemfd.DecRef(childTask)
	childOverlayFile.DecRef(childTask)
	childTruncateFile.DecRef(childTask)
	childDevMountFile.DecRef(childTask)
	childTmpfsUnlinked.DecRef(childTask)
	childTmpfsTruncate.DecRef(childTask)
	childManagedVolume.DecRef(childTask)
	childManagedAlias.DecRef(childTask)
	childConsoleStdin.DecRef(childTask)
	childConsoleStdout.DecRef(childTask)
	childReadonlyFile.DecRef(childTask)
	childReadonlyDir.DecRef(childTask)
	if err := l.family.Pause(childInfo.ID); err != nil {
		t.Fatalf("pausing child: %v", err)
	}
	if err := l.family.Resume(childInfo.ID); err != nil {
		t.Fatalf("resuming child: %v", err)
	}
	var childProcesses []*control.Process
	if err := l.family.Processes(childInfo.ID, l.root.cid, &childProcesses); err != nil {
		t.Fatalf("listing child processes: %v", err)
	}
	if len(childProcesses) != 1 || childProcesses[0].PID != 1 {
		t.Fatalf("child processes: got %+v, want virtual PID 1", childProcesses)
	}
	var childListed *BranchInfo
	for _, listed := range l.family.List() {
		if listed.ID == childInfo.ID {
			snapshot := listed
			childListed = &snapshot
			break
		}
	}
	if childListed == nil || childListed.InitPID != 1 || childListed.InitState != string(BranchRunning) || !childListed.SharesMemory {
		t.Errorf("live child diagnostics: %+v", childListed)
	}
	if err := l.family.Signal(childInfo.ID, l.root.cid, 0, int32(linux.SIGKILL), DeliverToProcess); err != nil {
		t.Fatalf("signaling child init: %v", err)
	}
	status, err := l.family.Wait(childInfo.ID, l.root.cid, 0)
	if err != nil {
		t.Fatalf("waiting for child init: %v", err)
	}
	ws := linux.WaitStatus(status)
	if !ws.Signaled() || ws.TerminationSignal() != linux.SIGKILL {
		t.Errorf("child wait status: got %v, want SIGKILL", ws)
	}
	// PID 1 has left the live PID table after the first wait, but repeated
	// lifecycle operations must still observe its retained terminal status.
	statusAgain, err := l.family.Wait(childInfo.ID, l.root.cid, 0)
	if err != nil {
		t.Fatalf("waiting for exited child init again: %v", err)
	}
	if statusAgain != status {
		t.Errorf("second child wait status: got %v, want %v", linux.WaitStatus(statusAgain), ws)
	}
	for _, listed := range l.family.List() {
		if listed.ID == childInfo.ID && (listed.InitPID != 1 || listed.InitState != "exited") {
			t.Errorf("exited child diagnostics: %+v", listed)
		}
	}
	if got := l.k.GlobalInit().Leader().ExitState(); got != kernel.TaskExitNone {
		t.Errorf("parent init changed after signaling child: got exit state %v", got)
	}
	if err := l.family.Destroy(childInfo.ID); err != nil {
		t.Fatalf("destroying child before grandchild: %v", err)
	}

	// The grandchild owns a complete reference closure and remains functional
	// after its source Kernel has been released.
	liveForkTestMMRead(t, grandchildTask, shmAddr1, "grnd")
	liveForkTestPRead(t, grandchildTask, grandchildMemfd, 0, "grnd")
	liveForkTestPRead(t, grandchildTask, grandchildOverlayFile, 0, "grnd")
	liveForkTestPRead(t, grandchildTask, grandchildDevMountFile, 0, "grnd")
	liveForkTestPRead(t, grandchildTask, grandchildTmpfsUnlinked, 0, "GUNL")
	liveForkTestPRead(t, grandchildTask, grandchildTmpfsTruncate, 0, "se")
	liveForkTestMMReadContext(t, grandchildCtx, grandchildTask, readonlyMapAddr, "hoMUTATED")
	liveForkTestRead(t, grandchildCtx, grandchildReadonlyFile, "D")
	_ = liveForkTestNextDirent(t, grandchildCtx, grandchildReadonlyDir)
	liveForkTestRead(t, ctx, parentReadonlyFile, "TE")
	if got := liveForkTestNextDirent(t, ctx, parentReadonlyDir); got != recursiveDirent {
		t.Fatalf("parent read-only bind cursor changed with descendant activity: got %q, want %q", got, recursiveDirent)
	}
	liveForkTestPathRead(t, grandchildTask, "readonly/fixture", "hoMUTATED")
	liveForkTestWrite(t, grandchildTask, grandchildUDPClient, "stay")
	liveForkTestRead(t, grandchildTask, grandchildUDPServer, "stay")
	liveForkTestWrite(t, grandchildTask, grandchildTCPClient, "stay")
	liveForkTestPRead(t, grandchildTask, grandchildManagedVolume, 0, "grnd")
	liveForkTestPRead(t, grandchildTask, grandchildManagedAlias, 0, "grnd")
	liveForkTestWrite(t, grandchildTask, grandchildConsoleStdout, "survivor-output")
	liveForkTestConsoleRead(t, grandchild.k, grandchildTask, internalConsoleID, kernel.InternalConsoleStdout, "survivor-output")
	if err := grandchild.k.CloseInternalConsoleStdin(grandchildTask, internalConsoleID); err != nil {
		t.Fatalf("closing grandchild internal console stdin: %v", err)
	}
	liveForkTestEOF(t, grandchildTask, grandchildConsoleStdin)
	liveForkTestReadEventually(t, grandchildTask, grandchildTCPServer, "stay")
	grandchildMemfd.DecRef(grandchildTask)
	grandchildOverlayFile.DecRef(grandchildTask)
	grandchildDevMountFile.DecRef(grandchildTask)
	grandchildTmpfsUnlinked.DecRef(grandchildTask)
	grandchildTmpfsTruncate.DecRef(grandchildTask)
	grandchildUDPClient.DecRef(grandchildTask)
	grandchildUDPServer.DecRef(grandchildTask)
	grandchildTCPClient.DecRef(grandchildTask)
	grandchildTCPServer.DecRef(grandchildTask)
	grandchildManagedVolume.DecRef(grandchildTask)
	grandchildManagedAlias.DecRef(grandchildTask)
	grandchildReadonlyFile.DecRef(grandchildTask)
	grandchildReadonlyDir.DecRef(grandchildTask)
	grandchildConsoleStdin.DecRef(grandchildTask)
	grandchildConsoleStdout.DecRef(grandchildTask)
	if err := l.family.Destroy(grandchildInfo.ID); err != nil {
		t.Fatalf("destroying grandchild: %v", err)
	}

	// Exercise the only other destruction ordering available for a three-level
	// family. The preceding chain deleted its middle ancestor first; this chain
	// deletes the leaf first, then its source. Every admitted OCI mount class
	// must remain usable from the surviving branch after each release.
	assertMountClosure := func(t *testing.T, branch *sandboxBranch) {
		t.Helper()
		task := branch.k.GlobalInit().Leader()
		branchCtx := branch.k.SupervisorContext()
		for _, path := range []string{"proc/1/status", "sys"} {
			file := liveForkTestOpenPath(t, task, path, linux.O_RDONLY)
			file.DecRef(branchCtx)
		}
		pty := liveForkTestOpenPath(t, task, "dev/ptmx", linux.O_RDWR|linux.O_NONBLOCK)
		pty.DecRef(branchCtx)
		liveForkTestPathRead(t, task, overlayPath, "prnt")
		liveForkTestPathRead(t, task, "dev/shm/ttc-mount-state", "prnt")
		liveForkTestPathRead(t, task, "managed/state", "prnt")
		liveForkTestPathRead(t, task, "managed-alias/state", "prnt")
		liveForkTestPathRead(t, task, "readonly/fixture", "hoMUTATED")
		liveForkTestPathWriteDenied(t, task, "readonly/fixture")
		liveForkTestPathRead(t, task, "etc/hostname", originalLower)
	}

	reverseChildInfo, err := l.family.Fork(&ForkBranchArgs{ID: "reverse-child", Paused: true})
	if err != nil {
		t.Fatalf("forking reverse-order child: %v", err)
	}
	reverseChild, err := l.family.branch(reverseChildInfo.ID)
	if err != nil {
		t.Fatalf("looking up reverse-order child: %v", err)
	}
	reverseGrandchildInfo, err := l.family.Fork(&ForkBranchArgs{Source: reverseChildInfo.ID, ID: "reverse-grandchild", Paused: true})
	if err != nil {
		t.Fatalf("forking reverse-order grandchild: %v", err)
	}
	reverseGrandchild, err := l.family.branch(reverseGrandchildInfo.ID)
	if err != nil {
		t.Fatalf("looking up reverse-order grandchild: %v", err)
	}
	t.Run("descendant-first grandchild", func(t *testing.T) {
		assertMountClosure(t, reverseGrandchild)
	})
	if err := l.family.Destroy(reverseGrandchildInfo.ID); err != nil {
		t.Fatalf("destroying reverse-order grandchild first: %v", err)
	}
	t.Run("descendant-first surviving child", func(t *testing.T) {
		assertMountClosure(t, reverseChild)
	})
	if err := l.family.Destroy(reverseChildInfo.ID); err != nil {
		t.Fatalf("destroying reverse-order child second: %v", err)
	}
	defaultBranch, err := l.family.branch(DefaultBranchID)
	if err != nil {
		t.Fatalf("looking up default branch after reverse-order destruction: %v", err)
	}
	t.Run("descendant-first surviving parent", func(t *testing.T) {
		assertMountClosure(t, defaultBranch)
	})
	parentReader.DecRef(ctx)
	parentWriter.DecRef(ctx)
	parentEvent.DecRef(ctx)
	parentEpollFile.DecRef(ctx)
	parentTimerFile.DecRef(ctx)
	parentSignalFile.DecRef(ctx)
	parentInotifyFile.DecRef(ctx)
	parentRoot.DecRef(ctx)
	parentUnix1.DecRef(parentTask)
	parentUnix2.DecRef(parentTask)
	parentUDPClient.DecRef(parentTask)
	parentUDPServer.DecRef(parentTask)
	parentTCPClient.DecRef(parentTask)
	parentTCPServer.DecRef(parentTask)
	parentNetlink.DecRef(parentTask)
	parentMemfd.DecRef(parentTask)
	parentOverlayFile.DecRef(parentTask)
	parentTruncateFile.DecRef(parentTask)
	parentDevMountFile.DecRef(parentTask)
	parentTmpfsUnlinked.DecRef(parentTask)
	parentTmpfsTruncate.DecRef(parentTask)
	parentMemfd = liveForkTestFD(t, l.k, memfdFD)
	liveForkTestPRead(t, parentTask, parentMemfd, 0, "prnt")
	parentMemfd.DecRef(parentTask)

	parentManagedVolume.DecRef(parentTask)
	parentManagedAlias.DecRef(parentTask)
	parentReadonlyFile.DecRef(parentTask)
	parentReadonlyDir.DecRef(parentTask)
	parentConsoleStdin.DecRef(parentTask)
	parentConsoleStdout.DecRef(parentTask)
	parentConsoleStderr.DecRef(parentTask)
	l.k.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	l.k.Unpause()
	l.k.WaitExited()
	select {
	case result := <-preStartWait:
		if result.err != nil {
			t.Fatalf("pre-start wait failed: %v", result.err)
		}
		ws := linux.WaitStatus(result.status)
		if !ws.Signaled() || ws.TerminationSignal() != linux.SIGKILL {
			t.Errorf("pre-start wait status: got %v, want SIGKILL", ws)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pre-start wait did not complete after init exited")
	}
}

func liveForkTestFD(t *testing.T, k *kernel.Kernel, fd int32) *vfs.FileDescription {
	t.Helper()
	return liveForkTestTaskFD(t, k.GlobalInit().Leader(), fd)
}

func liveForkTestTaskFD(t *testing.T, task *kernel.Task, fd int32) *vfs.FileDescription {
	t.Helper()
	file, _ := task.FDTable().Get(fd)
	if file == nil {
		t.Fatalf("FD %d is missing", fd)
	}
	return file
}

func liveForkTestNextDirent(t *testing.T, ctx context.Context, file *vfs.FileDescription) string {
	t.Helper()
	stop := errors.New("one dirent collected")
	var got string
	err := file.IterDirents(ctx, vfs.IterDirentsCallbackFunc(func(dirent vfs.Dirent) error {
		if got != "" {
			return stop
		}
		got = dirent.Name
		return nil
	}))
	if err != nil && !errors.Is(err, stop) {
		t.Fatalf("IterDirents: %v", err)
	}
	if got == "" {
		t.Fatal("IterDirents reached end of directory")
	}
	return got
}

func liveForkTestWrite(t *testing.T, ctx context.Context, file *vfs.FileDescription, value string) {
	t.Helper()
	n, err := file.Write(ctx, usermem.BytesIOSequence([]byte(value)), vfs.WriteOptions{})
	if err != nil || n != int64(len(value)) {
		t.Fatalf("Write(%q): got (%d, %v)", value, n, err)
	}
}

func liveForkTestRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, want string) {
	t.Helper()
	buf := make([]byte, len(want))
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if err != nil || n != int64(len(want)) || !bytes.Equal(buf, []byte(want)) {
		t.Fatalf("Read: got (%q, %d, %v), want %q", buf, n, err, want)
	}
}

func liveForkTestReadEventually(t *testing.T, ctx context.Context, file *vfs.FileDescription, want string) {
	t.Helper()
	buf := make([]byte, len(want))
	read := 0
	for attempts := 0; attempts < 10000 && read < len(buf); attempts++ {
		n, err := file.Read(ctx, usermem.BytesIOSequence(buf[read:]), vfs.ReadOptions{})
		read += int(n)
		if err != nil && !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
			t.Fatalf("Read: got (%q, %d, %v), want %q", buf[:read], read, err, want)
		}
		if read == len(buf) {
			if !bytes.Equal(buf, []byte(want)) {
				t.Fatalf("Read: got %q, want %q", buf, want)
			}
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("Read: timed out after reading %q, want %q", buf[:read], want)
}

func liveForkTestWaitReadable(t *testing.T, file *vfs.FileDescription) {
	t.Helper()
	for attempts := 0; attempts < 10000; attempts++ {
		if file.Readiness(waiter.EventIn)&waiter.EventIn != 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("timed out waiting for readable file")
}

func liveForkTestWouldBlock(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	buf := make([]byte, 1)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if n != 0 || !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		t.Fatalf("empty nonblocking Read: got (%d, %v), want (0, EWOULDBLOCK)", n, err)
	}
}

func liveForkTestEOF(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	buf := make([]byte, 1)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF Read: got (%d, %v), want (0, EOF)", n, err)
	}
}

func liveForkTestConsoleWriteInput(t *testing.T, k *kernel.Kernel, ctx context.Context, id, value string) {
	t.Helper()
	n, wouldBlock, err := k.WriteInternalConsole(ctx, id, []byte(value))
	if err != nil {
		t.Fatalf("writing internal console %q input: %v", id, err)
	}
	if wouldBlock || n != len(value) {
		t.Fatalf("writing internal console %q input: wrote %d/%d, wouldBlock=%t", id, n, len(value), wouldBlock)
	}
}

func liveForkTestConsoleRead(t *testing.T, k *kernel.Kernel, ctx context.Context, id string, stream kernel.InternalConsoleStream, want string) {
	t.Helper()
	data, eof, wouldBlock, err := k.ReadInternalConsole(ctx, id, stream, len(want))
	if err != nil {
		t.Fatalf("reading internal console %q stream %d: %v", id, stream, err)
	}
	if eof || wouldBlock || string(data) != want {
		t.Fatalf("reading internal console %q stream %d: got %q, eof=%t wouldBlock=%t, want %q", id, stream, data, eof, wouldBlock, want)
	}
}

func liveForkTestConsoleWouldBlock(t *testing.T, k *kernel.Kernel, ctx context.Context, id string, stream kernel.InternalConsoleStream) {
	t.Helper()
	data, eof, wouldBlock, err := k.ReadInternalConsole(ctx, id, stream, 1)
	if err != nil {
		t.Fatalf("probing internal console %q stream %d: %v", id, stream, err)
	}
	if eof || !wouldBlock || len(data) != 0 {
		t.Fatalf("probing internal console %q stream %d: got %q, eof=%t wouldBlock=%t", id, stream, data, eof, wouldBlock)
	}
}

func liveForkTestUDPPair(t *testing.T, task *kernel.Task, clientPort, serverPort uint16) (*vfs.FileDescription, *vfs.FileDescription) {
	t.Helper()
	newSocket := func(name string) *vfs.FileDescription {
		file, err := socket.New(task, linux.AF_INET, linux.SOCK_DGRAM, unix.IPPROTO_UDP)
		if err != nil {
			t.Fatalf("creating loopback UDP %s: %v", name, err)
		}
		if err := file.SetStatusFlags(task, task.Credentials(), file.StatusFlags()|linux.O_NONBLOCK); err != nil {
			file.DecRef(task)
			t.Fatalf("making loopback UDP %s nonblocking: %v", name, err)
		}
		return file
	}
	client := newSocket("client")
	server := newSocket("server")
	bind := func(name string, file *vfs.FileDescription, port uint16) {
		if err := file.Impl().(socket.Socket).Bind(task, liveForkTestIPv4Sockaddr(port)); err != nil {
			client.DecRef(task)
			server.DecRef(task)
			t.Fatalf("binding loopback UDP %s: %v", name, err)
		}
	}
	bind("client", client, clientPort)
	bind("server", server, serverPort)
	connect := func(name string, file *vfs.FileDescription, port uint16) {
		if err := file.Impl().(socket.Socket).Connect(task, liveForkTestIPv4Sockaddr(port), false); err != nil {
			client.DecRef(task)
			server.DecRef(task)
			t.Fatalf("connecting loopback UDP %s: %v", name, err)
		}
	}
	connect("client", client, serverPort)
	connect("server", server, clientPort)
	return client, server
}

func liveForkTestTCPPair(t *testing.T, task *kernel.Task, serverPort uint16) (*vfs.FileDescription, *vfs.FileDescription) {
	t.Helper()
	newSocket := func(name string) *vfs.FileDescription {
		file, err := socket.New(task, linux.AF_INET, linux.SOCK_STREAM, unix.IPPROTO_TCP)
		if err != nil {
			t.Fatalf("creating loopback TCP %s: %v", name, err)
		}
		if err := file.SetStatusFlags(task, task.Credentials(), file.StatusFlags()|linux.O_NONBLOCK); err != nil {
			file.DecRef(task)
			t.Fatalf("making loopback TCP %s nonblocking: %v", name, err)
		}
		return file
	}
	listener := newSocket("listener")
	listenerSocket := listener.Impl().(socket.Socket)
	if err := listenerSocket.Bind(task, liveForkTestIPv4Sockaddr(serverPort)); err != nil {
		listener.DecRef(task)
		t.Fatalf("binding loopback TCP listener: %v", err)
	}
	if err := listenerSocket.Listen(task, 1); err != nil {
		listener.DecRef(task)
		t.Fatalf("listening on loopback TCP socket: %v", err)
	}
	client := newSocket("client")
	if err := client.Impl().(socket.Socket).Connect(task, liveForkTestIPv4Sockaddr(serverPort), false); err != nil && !linuxerr.Equals(linuxerr.EINPROGRESS, err.ToError()) {
		listener.DecRef(task)
		client.DecRef(task)
		t.Fatalf("connecting loopback TCP client: %v", err)
	}

	acceptedFD := int32(-1)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fd, _, _, err := listenerSocket.Accept(task, false, linux.SOCK_NONBLOCK, false)
		if err == nil {
			acceptedFD = fd
			break
		}
		if !linuxerr.Equals(linuxerr.ErrWouldBlock, err.ToError()) {
			listener.DecRef(task)
			client.DecRef(task)
			t.Fatalf("accepting loopback TCP connection: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	listener.DecRef(task)
	if acceptedFD < 0 {
		client.DecRef(task)
		t.Fatal("loopback TCP connection was not accepted")
	}
	accepted := task.FDTable().Remove(task, acceptedFD)
	if accepted == nil {
		client.DecRef(task)
		t.Fatalf("accepted loopback TCP FD %d disappeared", acceptedFD)
	}
	return client, accepted
}

func liveForkTestIPv4Sockaddr(port uint16) []byte {
	addr := linux.SockAddrInet{
		Family: linux.AF_INET,
		Port:   socket.Htons(port),
		Addr:   linux.InetAddr{127, 0, 0, 1},
	}
	buf := make([]byte, addr.SizeBytes())
	addr.MarshalBytes(buf)
	return buf
}

func liveForkEventWrite(t *testing.T, ctx context.Context, file *vfs.FileDescription, value uint64) {
	t.Helper()
	var buf [8]byte
	binary.NativeEndian.PutUint64(buf[:], value)
	n, err := file.Write(ctx, usermem.BytesIOSequence(buf[:]), vfs.WriteOptions{})
	if err != nil || n != int64(len(buf)) {
		t.Fatalf("eventfd Write(%d): got (%d, %v)", value, n, err)
	}
}

func liveForkEventRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, want uint64) {
	t.Helper()
	var buf [8]byte
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf[:]), vfs.ReadOptions{})
	got := binary.NativeEndian.Uint64(buf[:])
	if err != nil || n != int64(len(buf)) || got != want {
		t.Fatalf("eventfd Read: got (%d, %d, %v), want %d", got, n, err, want)
	}
}

func liveForkCounterRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, want uint64, kind string) {
	t.Helper()
	var buf [8]byte
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf[:]), vfs.ReadOptions{})
	got := binary.NativeEndian.Uint64(buf[:])
	if err != nil || n != int64(len(buf)) || got != want {
		t.Fatalf("%s Read: got (%d, %d, %v), want %d", kind, got, n, err, want)
	}
}

func liveForkEventWouldBlock(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	var buf [8]byte
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf[:]), vfs.ReadOptions{})
	if n != 0 || !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		t.Fatalf("empty eventfd Read: got (%d, %v), want (0, EWOULDBLOCK)", n, err)
	}
}

func liveForkEpollEvents(t *testing.T, epoll *vfs.EpollInstance, want []linux.EpollEvent) {
	t.Helper()
	got := epoll.ReadEvents(nil, 16)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("epoll events mismatch (-want +got):\n%s", diff)
	}
}

func liveForkSignalRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, want linux.Signal) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	got := linux.Signal(binary.NativeEndian.Uint32(buf[:4]))
	if err != nil || n != int64(len(buf)) || got != want {
		t.Fatalf("signalfd Read: got (signal %d, %d, %v), want signal %d", got, n, err, want)
	}
}

func liveForkSignalWouldBlock(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if n != 0 || !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		t.Fatalf("empty signalfd Read: got (%d, %v), want (0, EWOULDBLOCK)", n, err)
	}
}

func liveForkInotifyRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, wantWD int32, wantMask uint32) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	gotWD := int32(binary.NativeEndian.Uint32(buf[0:4]))
	gotMask := binary.NativeEndian.Uint32(buf[4:8])
	if err != nil || n != 16 || gotWD != wantWD || gotMask != wantMask {
		t.Fatalf("inotify Read: got (wd %d, mask %#x, %d, %v), want (wd %d, mask %#x, 16, nil)", gotWD, gotMask, n, err, wantWD, wantMask)
	}
}

func liveForkInotifyWouldBlock(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if n != 0 || !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		t.Fatalf("empty inotify Read: got (%d, %v), want (0, EWOULDBLOCK)", n, err)
	}
}

func liveForkTestPWrite(t *testing.T, ctx context.Context, file *vfs.FileDescription, offset int64, value string) {
	t.Helper()
	n, err := file.PWrite(ctx, usermem.BytesIOSequence([]byte(value)), offset, vfs.WriteOptions{})
	if err != nil || n != int64(len(value)) {
		t.Fatalf("PWrite(%q, %d): got (%d, %v)", value, offset, n, err)
	}
}

func liveForkTestPRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, offset int64, want string) {
	t.Helper()
	buf := make([]byte, len(want))
	n, err := file.PRead(ctx, usermem.BytesIOSequence(buf), offset, vfs.ReadOptions{})
	if err != nil || n != int64(len(buf)) || string(buf) != want {
		t.Fatalf("PRead(%d): got (%q, %d, %v), want %q", offset, buf, n, err, want)
	}
}

func liveForkTestCreateFile(t *testing.T, task *kernel.Task, path string) *vfs.FileDescription {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	file, err := task.Kernel().VFS().OpenAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(path),
	}, &vfs.OpenOptions{
		Flags: linux.O_RDWR | linux.O_CREAT | linux.O_EXCL,
		Mode:  linux.ModeRegular | 0644,
	})
	if err != nil {
		t.Fatalf("creating %q: %v", path, err)
	}
	return file
}

func liveForkTestUnlink(t *testing.T, task *kernel.Task, path string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	if err := task.Kernel().VFS().UnlinkAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(path),
	}); err != nil {
		t.Fatalf("unlinking %q: %v", path, err)
	}
}

func liveForkTestLink(t *testing.T, task *kernel.Task, oldPath, newPath string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	oldPop := &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(oldPath)}
	newPop := &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(newPath)}
	if err := task.Kernel().VFS().LinkAt(ctx, task.Credentials(), oldPop, newPop); err != nil {
		t.Fatalf("linking %q to %q: %v", newPath, oldPath, err)
	}
}

func liveForkTestPathMissing(t *testing.T, task *kernel.Task, path string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	file, err := task.Kernel().VFS().OpenAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(path),
	}, &vfs.OpenOptions{Flags: linux.O_RDONLY})
	if file != nil {
		file.DecRef(ctx)
	}
	if !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Fatalf("opening missing path %q: got %v, want ENOENT", path, err)
	}
}

func liveForkTestPathRead(t *testing.T, task *kernel.Task, path, want string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	file := liveForkTestOpenPath(t, task, path, linux.O_RDONLY)
	defer file.DecRef(ctx)
	liveForkTestPRead(t, ctx, file, 0, want)
}

func liveForkTestOpenPath(t *testing.T, task *kernel.Task, path string, flags uint32) *vfs.FileDescription {
	t.Helper()
	return liveForkTestOpenPathContext(t, task.Kernel().SupervisorContext(), task, path, flags)
}

func liveForkTestOpenPathContext(t *testing.T, ctx context.Context, task *kernel.Task, path string, flags uint32) *vfs.FileDescription {
	t.Helper()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	file, err := task.Kernel().VFS().OpenAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:               root,
		Start:              root,
		Path:               fspath.Parse(path),
		FollowFinalSymlink: flags&linux.O_NOFOLLOW == 0,
	}, &vfs.OpenOptions{Flags: flags})
	if err != nil {
		t.Fatalf("opening %q: %v", path, err)
	}
	return file
}

func liveForkTestReadAllPathContext(t *testing.T, ctx context.Context, task *kernel.Task, path string) string {
	t.Helper()
	file := liveForkTestOpenPathContext(t, ctx, task, path, linux.O_RDONLY)
	readCtx := task.Kernel().SupervisorContext()
	defer file.DecRef(readCtx)
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := file.Read(readCtx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("reading %q: %v", path, err)
			}
			break
		}
		if n == 0 {
			break
		}
	}
	return out.String()
}

func liveForkTestPathWriteDenied(t *testing.T, task *kernel.Task, path string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	file, err := task.Kernel().VFS().OpenAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:               root,
		Start:              root,
		Path:               fspath.Parse(path),
		FollowFinalSymlink: true,
	}, &vfs.OpenOptions{Flags: linux.O_WRONLY})
	if file != nil {
		file.DecRef(ctx)
	}
	if err == nil {
		t.Fatalf("opening read-only path %q for write succeeded", path)
	}
	if !linuxerr.Equals(linuxerr.EROFS, err) && !linuxerr.Equals(linuxerr.EACCES, err) && !linuxerr.Equals(linuxerr.EPERM, err) {
		t.Fatalf("opening read-only path %q for write: got %v, want EROFS/EACCES/EPERM", path, err)
	}
}

func liveForkTestPReadString(t *testing.T, ctx context.Context, file *vfs.FileDescription, offset int64, length int) string {
	t.Helper()
	buf := make([]byte, length)
	n, err := file.PRead(ctx, usermem.BytesIOSequence(buf), offset, vfs.ReadOptions{})
	if err != nil || n != int64(length) {
		t.Fatalf("PRead(%d, %d): got (%q, %d, %v)", offset, length, buf, n, err)
	}
	return string(buf)
}

func liveForkTestMode(t *testing.T, ctx context.Context, file *vfs.FileDescription, want linux.FileMode) {
	t.Helper()
	stat, err := file.Stat(ctx, vfs.StatOptions{Mask: linux.STATX_MODE})
	if err != nil {
		t.Fatalf("Stat mode: %v", err)
	}
	if got := linux.FileMode(stat.Mode) & 0777; got != want {
		t.Fatalf("mode: got %#o, want %#o", got, want)
	}
}

func liveForkTestSize(t *testing.T, ctx context.Context, file *vfs.FileDescription, want uint64) {
	t.Helper()
	stat, err := file.Stat(ctx, vfs.StatOptions{Mask: linux.STATX_SIZE})
	if err != nil {
		t.Fatalf("Stat size: %v", err)
	}
	if stat.Size != want {
		t.Fatalf("size: got %d, want %d", stat.Size, want)
	}
}

func liveForkTestMMWrite(t *testing.T, task *kernel.Task, addr hostarch.Addr, value string) {
	t.Helper()
	if n, err := task.MemoryManager().CopyOut(task, addr, []byte(value), usermem.IOOpts{}); err != nil || n != len(value) {
		t.Fatalf("MemoryManager.CopyOut(%#x, %q): got (%d, %v)", addr, value, n, err)
	}
}

func liveForkTestMMRead(t *testing.T, task *kernel.Task, addr hostarch.Addr, want string) {
	t.Helper()
	liveForkTestMMReadContext(t, task, task, addr, want)
}

func liveForkTestMMReadContext(t *testing.T, ctx context.Context, task *kernel.Task, addr hostarch.Addr, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if n, err := task.MemoryManager().CopyIn(ctx, addr, got, usermem.IOOpts{}); err != nil || n != len(got) {
		t.Fatalf("MemoryManager.CopyIn(%#x): got (%q, %d, %v)", addr, got, n, err)
	}
	if string(got) != want {
		t.Fatalf("memory at %#x: got %q, want %q", addr, got, want)
	}
}

func liveForkTestMappedWrite(t *testing.T, ctx context.Context, file *vfs.FileDescription, value string) {
	t.Helper()
	opts := memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.ReadWrite,
	}
	if err := file.ConfigureMMap(ctx, &opts); err != nil {
		t.Fatalf("ConfigureMMap: %v", err)
	}
	defer opts.MappingIdentity.DecRef(ctx)
	mr := memmap.MappableRange{Start: 0, End: hostarch.PageSize}
	translations, err := opts.Mappable.Translate(ctx, mr, mr, hostarch.Write)
	if err != nil || len(translations) != 1 {
		t.Fatalf("Translate: got (%d translations, %v), want (1, nil)", len(translations), err)
	}
	translation := translations[0]
	fr := memmap.FileRange{Start: translation.Offset, End: translation.Offset + uint64(len(value))}
	blocks, err := translation.File.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	n, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice([]byte(value))))
	if err != nil || n != uint64(len(value)) {
		t.Fatalf("mapped write %q: got (%d, %v)", value, n, err)
	}
}

// TestStartSignal tests that the controller Start message will cause
// WaitForStartSignal to return.
func TestStartSignal(t *testing.T) {
	l, cleanup, err := createLoader(testConfig(), testSpec())
	if err != nil {
		t.Fatalf("error creating loader: %v", err)
	}
	defer l.Destroy()
	defer cleanup()

	// We aren't going to wait on this application, so the control server
	// needs to be shut down manually.
	defer l.ctrl.srv.Stop(time.Hour)

	// Start a goroutine that calls WaitForStartSignal and writes to a
	// channel when it returns.
	waitFinished := make(chan struct{})
	go func() {
		l.WaitForStartSignal()
		// Pretend that Run() executed and returned no error.
		l.ctrl.manager.startResultChan <- nil
		waitFinished <- struct{}{}
	}()

	// Nothing has been written to the channel, so waitFinished should not
	// return.  Give it a little bit of time to make sure the goroutine has
	// started.
	select {
	case <-waitFinished:
		t.Errorf("WaitForStartSignal completed but it should not have")
	case <-time.After(50 * time.Millisecond):
		// OK.
	}

	// Trigger the control server StartRoot method.
	cid := "foo"
	if err := l.ctrl.manager.StartRoot(&cid, nil); err != nil {
		t.Errorf("error calling StartRoot: %v", err)
	}

	// Now WaitForStartSignal should return (within a short amount of
	// time).
	select {
	case <-waitFinished:
		// OK.
	case <-time.After(50 * time.Millisecond):
		t.Errorf("WaitForStartSignal did not complete but it should have")
	}
}

// Test that network=host with raw sockets enabled requires CAP_NET_RAW on the
// host.
func TestHostnetWithRawSockets(t *testing.T) {
	// Capabilities are per-thread. Pin before saving or changing them.
	runtime.LockOSThread()
	restoreFailed := false
	t.Cleanup(func() {
		if !restoreFailed {
			runtime.UnlockOSThread()
		}
	})

	// Drop CAP_NET_RAW from effective capabilities, if we have it.
	pid := os.Getpid()
	caps, err := capability.NewPid2(0)
	if err != nil {
		t.Fatalf("error getting capabilities for pid %d: %v", pid, err)
	}
	if err := caps.Load(); err != nil {
		t.Fatalf("error loading capabilities: %v", err)
	}
	if caps.Get(capability.EFFECTIVE, capability.CAP_NET_RAW) {
		caps.Unset(capability.EFFECTIVE, capability.CAP_NET_RAW)
		if err := caps.Apply(capability.CAPS); err != nil {
			t.Fatalf("error applying capabilities: %v", err)
		}
		t.Cleanup(func() {
			caps.Set(capability.EFFECTIVE, capability.CAP_NET_RAW)
			if err := caps.Apply(capability.CAPS); err != nil {
				// Do not return a thread with altered capabilities to the runtime.
				restoreFailed = true
				t.Errorf("error restoring capabilities: %v", err)
			}
		})
	}

	// Configure host network with raw sockets.
	conf := testConfig()
	conf.Network = config.NetworkHost
	conf.EnableRaw = true

	// Creating loader should fail.
	l, err := New(Args{
		ID:              "should-fail",
		Spec:            testSpec(),
		Conf:            conf,
		DevGoferFD:      -1,
		PodInitConfigFD: -1,
		ExecFD:          -1,
		CPUDMALatencyFD: -1,
	})
	if err == nil {
		l.Destroy()
		t.Fatalf("expected loader.New() to fail but it did not")
	}
	// Error message must be about CAP_NET_RAW.
	if !strings.Contains(err.Error(), "CAP_NET_RAW") {
		t.Errorf("expected error to contain CAP_NET_RAW but got %q", err)
	}
}

type CreateMountTestcase struct {
	name string
	// Spec that will be used to create the mount manager.  Note
	// that we can't mount procfs without a kernel, so each spec
	// MUST contain something other than procfs mounted at /proc.
	spec specs.Spec
	// Paths that are expected to exist in the resulting fs.
	expectedPaths []string
}

func createMountTestcases() []*CreateMountTestcase {
	testCases := []*CreateMountTestcase{
		{
			// Only proc.
			name: "only proc mount",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
				},
			},
			// /proc, /dev, and /sys should always be mounted.
			expectedPaths: []string{"/proc", "/dev", "/sys"},
		},
		{
			// Mount at a deep path, with many components that do
			// not exist in the root.
			name: "deep mount path",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/some/very/very/deep/path",
						Type:        "tmpfs",
					},
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
				},
			},
			// /some/deep/path should be mounted, along with /proc, /dev, and /sys.
			expectedPaths: []string{"/some/very/very/deep/path", "/proc", "/dev", "/sys"},
		},
		{
			// Mounts are nested inside each other.
			name: "nested mounts",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
					{
						Destination: "/foo",
						Type:        "tmpfs",
					},
					{
						Destination: "/foo/qux",
						Type:        "tmpfs",
					},
					{
						// File mounts with the same prefix.
						Destination: "/foo/qux-quz",
						Type:        "tmpfs",
					},
					{
						Destination: "/foo/bar",
						Type:        "tmpfs",
					},
					{
						Destination: "/foo/bar/baz",
						Type:        "tmpfs",
					},
					{
						// A deep path that is in foo but not the other mounts.
						Destination: "/foo/some/very/very/deep/path",
						Type:        "tmpfs",
					},
				},
			},
			expectedPaths: []string{"/foo", "/foo/bar", "/foo/bar/baz", "/foo/qux",
				"/foo/qux-quz", "/foo/some/very/very/deep/path", "/proc", "/dev", "/sys"},
		},
		{
			name: "mount inside /dev",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
					{
						Destination: "/dev",
						Type:        "tmpfs",
					},
					{
						// Mount with the same prefix.
						Destination: "/dev/fd-foo",
						Type:        "tmpfs",
					},
					{
						// Unsupported fs type.
						Destination: "/dev/mqueue",
						Type:        "mqueue",
					},
					{
						Destination: "/dev/foo",
						Type:        "tmpfs",
					},
					{
						Destination: "/dev/bar",
						Type:        "tmpfs",
					},
				},
			},
			expectedPaths: []string{"/proc", "/dev", "/dev/fd-foo", "/dev/foo", "/dev/bar", "/sys"},
		},
		{
			name: "mounts inside mandatory mounts",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
					{
						Destination: "/sys/bar",
						Type:        "tmpfs",
					},
					{
						Destination: "/tmp/baz",
						Type:        "tmpfs",
					},
					{
						Destination: "/dev/goo",
						Type:        "tmpfs",
					},
				},
			},
			expectedPaths: []string{"/proc", "/sys", "/sys/bar", "/tmp", "/tmp/baz", "/dev/goo"},
		},
	}

	return testCases
}

// Test that MountNamespace can be created with various specs.
func TestCreateMountNamespace(t *testing.T) {
	for _, tc := range createMountTestcases() {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			spec.Mounts = tc.spec.Mounts
			spec.Root = tc.spec.Root

			t.Logf("Using root: %q", spec.Root.Path)
			l, loaderCleanup, err := createLoader(testConfig(), spec)
			if err != nil {
				t.Fatalf("failed to create loader: %v", err)
			}
			defer l.Destroy()
			defer loaderCleanup()

			l.mu.Lock()
			defer l.mu.Unlock()
			mntr := l.newContainerMounter(&l.root)
			ctx := l.k.SupervisorContext()
			creds := auth.NewRootCredentials(l.root.procArgs.Credentials.UserNamespace)
			mns, err := mntr.mountAll(ctx, creds, l.root.spec, l.root.conf, &l.root.procArgs)
			if err != nil {
				t.Fatalf("mountAll: %v", err)
			}

			root := mns.Root(ctx)
			defer root.DecRef(ctx)
			for _, p := range tc.expectedPaths {
				target := &vfs.PathOperation{
					Root:  root,
					Start: root,
					Path:  fspath.Parse(p),
				}

				if d, err := l.k.VFS().GetDentryAt(ctx, l.root.procArgs.Credentials, target, &vfs.GetDentryOptions{}); err != nil {
					t.Errorf("expected path %v to exist with spec %v, but got error %v", p, tc.spec, err)
				} else {
					d.DecRef(ctx)
				}
			}
		})
	}
}

type createMountPointTestCase struct {
	name string
	path string
	mode linux.FileMode
	fail bool
}

func TestCreateMountPoint(t *testing.T) {
	createMountPointTestCases := []createMountPointTestCase{
		{
			name: "File",
			path: "/test/test-file",
			mode: linux.ModeRegular,
		},
		{
			name: "Directory",
			path: "/test/test-dir",
			mode: linux.ModeDirectory,
		},
		{
			name: "FileWithIntDirs",
			path: "/test/a/b/c/test-file",
			mode: linux.ModeRegular,
		},
		{
			name: "DirectoryWithIntDirs",
			path: "/test/d/e/f/g/test-dir",
			mode: linux.ModeDirectory,
		},
		{
			name: "ExistingFile",
			path: "/test/test-file",
			mode: linux.ModeRegular,
		},
		{
			name: "ExistingDirectory",
			path: "/test/test-dir",
			mode: linux.ModeDirectory,
		},
		{
			name: "DirVSFile",
			path: "/test/test-file",
			mode: linux.ModeDirectory,
			fail: true,
		},
		{
			name: "FileVSDir",
			path: "/test/test-dir",
			mode: linux.ModeRegular,
			fail: true,
		},
	}

	spec := testSpec()
	spec.Root = &specs.Root{
		Path: os.TempDir(),
	}
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: "/test",
		Type:        "tmpfs",
	})

	t.Logf("Using root: %q", spec.Root.Path)
	l, loaderCleanup, err := createLoader(testConfig(), spec)
	if err != nil {
		t.Fatalf("failed to create loader: %v", err)
	}
	defer l.Destroy()
	defer loaderCleanup()

	l.mu.Lock()
	defer l.mu.Unlock()
	mntr := l.newContainerMounter(&l.root)
	ctx := l.k.SupervisorContext()
	creds := auth.NewRootCredentials(l.root.procArgs.Credentials.UserNamespace)
	mns, err := mntr.mountAll(ctx, creds, l.root.spec, l.root.conf, &l.root.procArgs)
	if err != nil {
		t.Fatalf("mountAll: %v", err)
	}

	root := mns.Root(ctx)
	defer root.DecRef(ctx)

	for _, tc := range createMountPointTestCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := mntr.makeMountPoint(ctx, creds, mns, tc.path, tc.mode); err != nil {
				if tc.fail {
					return
				}
				t.Fatalf("makeMountPoint failed: %v", err)
			} else {
				if tc.fail {
					t.Fatalf("makeMountPoint doesn't fail")
				}
			}
			target := &vfs.PathOperation{
				Root:  root,
				Start: root,
				Path:  fspath.Parse(tc.path),
			}

			if mode, err := mntr.getPathMode(ctx, creds, target); err != nil {
				t.Errorf("expected path %v, but got error %v", tc.path, err)
			} else {
				if mode.IsDir() != tc.mode.IsDir() {
					t.Errorf("path mode %v doesn't match the mount mode %v", mode, tc.mode)
				}
			}
		})
	}
}

func TestMain(m *testing.M) {
	cpuid.Initialize()
	seccheck.Initialize()
	os.Exit(m.Run())
}

func TestNetworkConfig(t *testing.T) {
	l, cleanup, err := createLoader(testConfig(), testSpec())
	if err != nil {
		t.Fatalf("error creating loader: %v", err)
	}
	defer l.Destroy()
	defer cleanup()

	// We aren't going to wait on this application, so the control server
	// needs to be shut down manually.
	defer l.ctrl.srv.Stop(time.Hour)

	go func() {
		l.WaitForStartSignal()
		l.ctrl.manager.startResultChan <- nil
	}()

	args := &CreateLinksAndRoutesArgs{
		LoopbackLinks: []LoopbackLink{
			{
				Name: "lo",
				Addresses: []IPWithPrefix{
					{Address: net.IP("\x7f\x00\x00\x01"), PrefixLen: 8},
				},
				Routes: []Route{
					{
						Destination: net.IPNet{
							IP:   net.IP{127, 0, 0, 0},
							Mask: net.IPMask{255, 0, 0, 0},
						},
					},
				},
			},
		},
	}
	if err := l.ctrl.manager.SetNetworkArgs(args, nil); err != nil {
		t.Errorf("error calling SetNetworkConfig: %v", err)
	}
	var networkArgs CreateLinksAndRoutesArgs
	if err := l.ctrl.manager.GetNetworkConfig(nil, &networkArgs); err != nil {
		t.Errorf("error calling NetworkConfig: %v", err)
	}
	if diff := cmp.Diff(networkArgs.LoopbackLinks, args.LoopbackLinks); diff != "" {
		t.Errorf("Network config content mismatch (-want +got):\n%s", diff)
	}
}

func TestSignalUnkillablePolicyMapping(t *testing.T) {
	for _, tc := range []struct {
		confPolicy   config.SignalUnkillablePolicy
		kernelPolicy kernel.SignalUnkillablePolicy
	}{
		{config.SignalUnkillableNone, kernel.SignalUnkillableNone},
		{config.SignalUnkillableLinux, kernel.SignalUnkillableLinux},
		{config.SignalUnkillablePolicy(-1), kernel.SignalUnkillableNone},
	} {
		if got := signalUnkillablePolicy(tc.confPolicy); got != tc.kernelPolicy {
			t.Errorf("signalUnkillablePolicy(%v) = %v, want %v", tc.confPolicy, got, tc.kernelPolicy)
		}
	}
}
