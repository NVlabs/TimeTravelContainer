// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/runsc/ttc/protocol"
)

func TestTerminateStartedDaemonKillsStartupSession(t *testing.T) {
	command := exec.Command("sh", "-c", "trap '' TERM; sleep 60")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	started := time.Now()
	terminateStartedDaemon(command, done)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("startup cleanup took %v", elapsed)
	}
	if err := command.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("failed daemon startup process survived cleanup")
	}
}

func TestHelpAndCompletionAreOffline(t *testing.T) {
	root := t.TempDir()
	socket := protocol.DaemonSocket(root)
	t.Setenv("TTC_ROOT", root)
	t.Setenv("TTC_LOCAL_ROOT", "")
	for _, args := range [][]string{{"--help"}, {"run", "--help"}, {"completion", "bash"}} {
		var stdout, stderr bytes.Buffer
		if status := Run(args, strings.NewReader(""), &stdout, &stderr); status != 0 || stdout.Len() == 0 || stderr.Len() != 0 {
			t.Fatalf("Run(%q): status=%d stdout=%q stderr=%q", args, status, stdout.String(), stderr.String())
		}
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("offline help/completion touched daemon socket %q: %v", socket, err)
	}
}

func TestHelpRejectsUnknownCommands(t *testing.T) {
	for _, args := range [][]string{{"branch", "--help"}, {"banana", "-h"}, {"help", "banana"}} {
		var stdout, stderr bytes.Buffer
		if status := Run(args, strings.NewReader(""), &stdout, &stderr); status != 2 {
			t.Fatalf("Run(%q): status=%d, want 2", args, status)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "unknown command") {
			t.Fatalf("Run(%q): stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestDetailedHelpIsClientLocal(t *testing.T) {
	// Help needs no daemon, nor a root to find one by.
	t.Setenv("TTC_ROOT", "")
	t.Setenv("TTC_LOCAL_ROOT", "")
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"daemon", "--help"}, []string{"Usage: ttc daemon COMMAND", "Commands:", "request inspect ID"}},
		{[]string{"daemon", "start", "--help"}, []string{"Usage: ttc daemon start", "TTC_ROOT", "starts alike", "--control-only", "512MiB/8GiB", "default: 30s", "does not advertise a TCP endpoint"}},
		{[]string{"help", "daemon", "stop"}, []string{"Usage: ttc daemon stop", "build mismatches"}},
		{[]string{"image", "--help"}, []string{"Manage Docker/OCI images", "load [OPTIONS]", "rm [OPTIONS]"}},
		{[]string{"exec", "--help"}, []string{"named detached session", "--session NAME", "--process FILE"}},
		{[]string{"node", "--help"}, []string{"distributed TTC cluster", "rm NODE", "offline", "rebalance NODE"}},
		{[]string{"run", "--help"}, []string{"--memory SIZE", "default: 64m", "HOST_PORT:CONTAINER_PORT", "default: image user"}},
		{[]string{"logs", "--help"}, []string{"default: all", "relative duration", "default: false"}},
		{[]string{"tty", "--help"}, []string{"terminal state", "watch"}},
	}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		if status := Run(test.args, strings.NewReader(""), &stdout, &stderr); status != 0 {
			t.Fatalf("Run(%q): status=%d stderr=%q", test.args, status, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("Run(%q): stderr=%q", test.args, stderr.String())
		}
		for _, want := range test.want {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("Run(%q): help missing %q:\n%s", test.args, want, stdout.String())
			}
		}
	}
}

func TestClientRejectsRemovedRootAndDirectOptions(t *testing.T) {
	for _, option := range []string{"--root=/tmp/runtime", "--runsc=/bin/ttcd", "--data-root=/tmp/data", "--direct"} {
		var stdout, stderr bytes.Buffer
		if status := Run([]string{option, "ps"}, strings.NewReader(""), &stdout, &stderr); status != 2 || !strings.Contains(stderr.String(), "unknown global option") {
			t.Fatalf("removed option %q: status=%d stderr=%q", option, status, stderr.String())
		}
	}
}

func TestDaemonSocketIsInItsRoot(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name  string
		env   map[string]string
		args  []string
		want  string
		wantS int
	}{
		{name: "standalone", env: map[string]string{"TTC_ROOT": root}, args: []string{"ps"}, want: protocol.DaemonSocket(root), wantS: 1},
		{name: "cluster node", env: map[string]string{"TTC_ROOT": "/shared", "TTC_LOCAL_ROOT": filepath.Join(root, "local")}, args: []string{"ps"}, want: protocol.DaemonSocket(filepath.Join(root, "local")), wantS: 1},
		{name: "override", env: map[string]string{"TTC_ROOT": root}, args: []string{"--daemon-socket", filepath.Join(root, "other.sock"), "ps"}, want: filepath.Join(root, "other.sock"), wantS: 1},
		{name: "unset", env: map[string]string{}, args: []string{"ps"}, want: "TTC_ROOT is not set", wantS: 2},
		{name: "override of daemon start", env: map[string]string{"TTC_ROOT": root}, args: []string{"--daemon-socket", filepath.Join(root, "other.sock"), "daemon", "start"}, want: "does not apply to daemon start", wantS: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, name := range []string{"TTC_ROOT", "TTC_LOCAL_ROOT"} {
				t.Setenv(name, test.env[name])
			}
			var stdout, stderr bytes.Buffer
			if status := Run(test.args, strings.NewReader(""), &stdout, &stderr); status != test.wantS || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("Run(%q): status=%d stderr=%q, want status %d and %q", test.args, status, stderr.String(), test.wantS, test.want)
			}
		})
	}
}

func TestDaemonStopRetriesReportedLegacyAPI(t *testing.T) {
	socket := filepath.Join(shortSocketDir(t), "ttcd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan request, 2)
	serverErr := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 2; attempt++ {
			conn, err := listener.Accept()
			if err != nil {
				serverErr <- err
				return
			}
			header, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				conn.Close()
				serverErr <- err
				return
			}
			var req request
			if err := json.Unmarshal(header, &req); err != nil {
				conn.Close()
				serverErr <- err
				return
			}
			requests <- req
			var writeMu sync.Mutex
			status := 1
			if attempt == 0 {
				err = writeFrame(conn, &writeMu, daemonFrameStderr, []byte("ttcd: unsupported API version 6 (server uses 4)\n"))
			} else {
				status = 0
				err = writeFrame(conn, &writeMu, daemonFrameStdout, []byte("stopped\n"))
			}
			var encodedStatus [4]byte
			binary.BigEndian.PutUint32(encodedStatus[:], uint32(status))
			if err == nil {
				err = writeFrame(conn, &writeMu, daemonFrameStatus, encodedStatus[:])
			}
			conn.Close()
			if err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}()

	var stdout, stderr bytes.Buffer
	if status := stopDaemon(socket, []string{"--stop-all"}, &stdout, &stderr); status != 0 {
		t.Fatalf("stopDaemon status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if stdout.String() != "stopped\n" || stderr.Len() != 0 {
		t.Fatalf("stop output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	first, second := <-requests, <-requests
	if first.Version != daemonAPIVersion || second.Version != 4 {
		t.Fatalf("stop request versions=(%d, %d), want (%d, 4)", first.Version, second.Version, daemonAPIVersion)
	}
	for index, req := range []request{first, second} {
		if req.Action != "stop" || req.ClientBuildID != "" || !req.ManageStop || len(req.Args) != 1 || req.Args[0] != "--stop-all" {
			t.Fatalf("stop request %d=%+v", index, req)
		}
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestReportedDaemonAPIVersion(t *testing.T) {
	for _, test := range []struct {
		diagnostic string
		want       int
		ok         bool
	}{
		{"ttcd: unsupported API version 6 (server uses 4)\n", 4, true},
		{"ttcd: unsupported API version", 0, false},
		{"ttcd: unsupported API version 6 (server uses 0)", 0, false},
		{fmt.Sprintf("ttcd: unsupported API version 6 (server uses %d)", 1<<16+1), 0, false},
	} {
		got, ok := reportedDaemonAPIVersion(test.diagnostic)
		if got != test.want || ok != test.ok {
			t.Errorf("reportedDaemonAPIVersion(%q)=(%d, %t), want (%d, %t)", test.diagnostic, got, ok, test.want, test.ok)
		}
	}
}

func TestDetachedTTYRequestKeepsTerminalControlStreamOpen(t *testing.T) {
	socket := filepath.Join(shortSocketDir(t), "ttcd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan request, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		header, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			serverErr <- err
			return
		}
		var req request
		if err := json.Unmarshal(header, &req); err != nil {
			serverErr <- err
			return
		}
		requests <- req
		var status [4]byte
		var writeMu sync.Mutex
		serverErr <- writeFrame(conn, &writeMu, daemonFrameStatus, status[:])
	}()
	status, err := runCommand(socket, []string{"exec", "-td", "--session", "build", "demo@main", "sh"}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil || status != 0 {
		t.Fatalf("runCommand status=%d err=%v", status, err)
	}
	req := <-requests
	if req.Stdin || !req.TerminalControl {
		t.Fatalf("detached TTY request stdin=%t terminal_control=%t", req.Stdin, req.TerminalControl)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeClientRelativePaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args  []string
		index int
	}{
		{[]string{"checkpoint", "demo@main", "state.ttc"}, 2},
		{[]string{"restore", "state.ttc"}, 1},
		{[]string{"cp", "local.txt", "demo@main:/tmp/local.txt"}, 1},
	} {
		got, err := normalizeHostPaths(test.args)
		if err != nil {
			t.Fatalf("normalizeHostPaths(%q): %v", test.args, err)
		}
		if !strings.Contains(got[test.index], cwd) {
			t.Fatalf("normalizeHostPaths(%q)=%q", test.args, got)
		}
	}
}

func TestPrepareImageArchiveIOUsesClientFiles(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.tar")
	if err := os.WriteFile(inputPath, []byte("archive-input"), 0600); err != nil {
		t.Fatal(err)
	}
	args, input, output, closeArchive, err := prepareImageArchiveIO(
		[]string{"image", "load", "--platform", "linux/amd64", "-i", inputPath}, strings.NewReader("wrong"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeArchive(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "image load --platform linux/amd64" || string(data) != "archive-input" || output != io.Discard || !commandUsesStdin(args) {
		t.Fatalf("load args=%q data=%q stdin=%t", args, data, commandUsesStdin(args))
	}

	outputPath := filepath.Join(dir, "output.tar")
	args, input, output, closeArchive, err = prepareImageArchiveIO(
		[]string{"save", "--output=" + outputPath, "busybox:1.36"}, strings.NewReader("stdin"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(output, "archive-output"); err != nil {
		t.Fatal(err)
	}
	if err := closeArchive(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "save busybox:1.36" || string(data) != "archive-output" || input == nil {
		t.Fatalf("save args=%q data=%q", args, data)
	}
}

func TestPrepareCheckpointArchiveIOUsesClientFiles(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "output.ttc")
	args, input, output, closeArchive, err := prepareCheckpointArchiveIO(
		[]string{"checkpoint", "demo@main", outputPath}, strings.NewReader("stdin"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(output, "checkpoint-output"); err != nil {
		t.Fatal(err)
	}
	if err := closeArchive(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "checkpoint demo@main -" || string(data) != "checkpoint-output" || input == nil {
		t.Fatalf("checkpoint args=%q data=%q", args, data)
	}

	inputPath := filepath.Join(dir, "input.ttc")
	if err := os.WriteFile(inputPath, []byte("checkpoint-input"), 0600); err != nil {
		t.Fatal(err)
	}
	args, input, output, closeArchive, err = prepareCheckpointArchiveIO(
		[]string{"restore", "--node", "node-b", "--name", "copy", inputPath}, strings.NewReader("wrong"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeArchive(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "restore --node node-b --name copy -" || string(data) != "checkpoint-input" || output != io.Discard || !commandUsesStdin(args) {
		t.Fatalf("restore args=%q data=%q stdin=%t", args, data, commandUsesStdin(args))
	}
}

// shortSocketDir returns a directory for a Unix socket whose path fits in
// sockaddr_un on every host; t.TempDir can exceed Darwin's 104 bytes.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestNormalizeEnvironmentImports(t *testing.T) {
	t.Setenv("TTC_TEST_CLIENT_ENV", "from-client")
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"run", "-e", "TTC_TEST_CLIENT_ENV", "alpine"}, []string{"run", "-e", "TTC_TEST_CLIENT_ENV=from-client", "alpine"}},
		{[]string{"create", "--name", "demo", "--env=TTC_TEST_CLIENT_ENV", "alpine"}, []string{"create", "--name", "demo", "--env=TTC_TEST_CLIENT_ENV=from-client", "alpine"}},
		{[]string{"container", "run", "-e=TTC_TEST_CLIENT_ENV", "alpine"}, []string{"container", "run", "-e=TTC_TEST_CLIENT_ENV=from-client", "alpine"}},
		{[]string{"run", "--env", "TTC_TEST_CLIENT_ENV=explicit", "alpine"}, []string{"run", "--env", "TTC_TEST_CLIENT_ENV=explicit", "alpine"}},
		{[]string{"run", "--env", "TTC_TEST_MISSING_ENV", "alpine"}, []string{"run", "--env", "TTC_TEST_MISSING_ENV", "alpine"}},
		{[]string{"run", "alpine", "sh", "-c", "echo", "--env", "TTC_TEST_CLIENT_ENV"}, []string{"run", "alpine", "sh", "-c", "echo", "--env", "TTC_TEST_CLIENT_ENV"}},
		{[]string{"exec", "-e", "TTC_TEST_CLIENT_ENV", "demo@main", "true"}, []string{"exec", "-e", "TTC_TEST_CLIENT_ENV", "demo@main", "true"}},
	} {
		if got := normalizeEnvironmentImports(test.args); !reflect.DeepEqual(got, test.want) {
			t.Errorf("normalizeEnvironmentImports(%q)=%q, want %q", test.args, got, test.want)
		}
	}
}

func TestExplainDaemonExecFailure(t *testing.T) {
	killed := exec.Command("/bin/sh", "-c", "kill -KILL $$").Run()
	explained := explainDaemonExecFailure("/nonexistent/ttcd", killed)
	if runtime.GOOS == "darwin" {
		if !strings.Contains(explained.Error(), "code signature") || !strings.Contains(explained.Error(), "codesign --force") {
			t.Errorf("killed ttcd explained as %q, want its code signature and how to sign it", explained)
		}
	} else if explained != killed {
		t.Errorf("killed ttcd explained as %q, want %q", explained, killed)
	}
	failed := exec.Command("/bin/sh", "-c", "exit 3").Run()
	if got := explainDaemonExecFailure("/nonexistent/ttcd", failed); got != failed {
		t.Errorf("failed ttcd explained as %q, want %q", got, failed)
	}
}
