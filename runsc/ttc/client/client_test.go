// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package client

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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/runsc/ttc/protocol"
	"gvisor.dev/gvisor/runsc/version"
)

func TestQuorumWaitHelperProcess(t *testing.T) {
	if os.Getenv("TTC_TEST_QUORUM_WAIT_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestQuorumWaitDaemonLifecycle(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "ttcd.sock")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(directory, "ttcd")
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.OpenFile(daemon, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := errors.Join(source.Close(), destination.Close())
	if copyErr != nil || closeErr != nil {
		t.Fatal(errors.Join(copyErr, closeErr))
	}
	command := exec.Command(daemon, "-test.run=TestQuorumWaitHelperProcess", "--", "--socket", socket)
	command.Env = append(os.Environ(), "TTC_TEST_QUORUM_WAIT_HELPER=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_, _ = command.Process.Wait()
	}()

	waiting := protocol.DaemonQuorumWait{PID: command.Process.Pid, APIVersion: daemonAPIVersion, BuildID: version.Version()}
	data, err := json.Marshal(waiting)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protocol.DaemonQuorumWaitPath(socket), data, 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := quorumWaitDaemon(socket); !ok || got != waiting {
		t.Fatalf("quorumWaitDaemon=(%+v,%t), want %+v,true", got, ok, waiting)
	}

	var stdout, stderr bytes.Buffer
	if status := Run([]string{"--daemon-socket", socket, "daemon", "status"}, strings.NewReader(""), &stdout, &stderr); status != 0 || !strings.Contains(stdout.String(), `"waiting_for_cluster_quorum":true`) || stderr.Len() != 0 {
		t.Fatalf("waiting status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	waiting.BuildID = "different-build"
	data, _ = json.Marshal(waiting)
	if err := os.WriteFile(protocol.DaemonQuorumWaitPath(socket), data, 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if status := Run([]string{"--daemon-socket", socket, "daemon", "start"}, strings.NewReader(""), &stdout, &stderr); status != 1 || !strings.Contains(stderr.String(), "does not match") {
		t.Fatalf("mismatched start=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if status := Run([]string{"--daemon-socket", socket, "daemon", "stop"}, strings.NewReader(""), &stdout, &stderr); status != 0 || !strings.Contains(stdout.String(), "ttcd stopped") || stderr.Len() != 0 {
		t.Fatalf("waiting stop=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

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
	socket := filepath.Join(t.TempDir(), "absent.sock")
	t.Setenv("TTC_SOCKET", socket)
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
	t.Setenv("TTC_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"daemon", "--help"}, []string{"Usage: ttc daemon COMMAND", "Commands:", "request inspect ID"}},
		{[]string{"daemon", "start", "--help"}, []string{"Usage: ttc daemon start", "TTC_ROOT", "--cluster-init", "--control-only", "512MiB/8GiB", "default: 15s", "does not advertise a TCP endpoint"}},
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

func TestTTCSocketAndOverride(t *testing.T) {
	t.Setenv("TTC_SOCKET", "/tmp/from-env.sock")
	if got := defaultSocket(); got != "/tmp/from-env.sock" {
		t.Fatalf("defaultSocket=%q", got)
	}
	opts, rest, done, err := parseGlobal([]string{"--daemon-socket", "/tmp/from-flag.sock", "ps"}, os.Stdout)
	if err != nil || done || opts.socket != "/tmp/from-flag.sock" || len(rest) != 1 || rest[0] != "ps" {
		t.Fatalf("parseGlobal=(%+v, %q, %t, %v)", opts, rest, done, err)
	}
}

func TestDaemonStopRetriesReportedLegacyAPI(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "ttcd.sock")
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

func TestNormalizeClientRelativePaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args  []string
		index int
	}{
		{[]string{"save", "-o", "image.tar", "busybox"}, 2},
		{[]string{"load", "--input=archive.tar"}, 1},
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
