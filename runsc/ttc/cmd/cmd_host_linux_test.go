// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmd

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A child's exit is awaited in the poller, not in a system call that holds a
// thread.
func TestAwaitExitWaitsInThePoller(t *testing.T) {
	// A kernel before 5.10, or a container's seccomp policy, has no pidfd
	// that polls, and awaitExit returns at once for a Wait to wait.
	fd, err := unix.PidfdOpen(os.Getpid(), unix.PIDFD_NONBLOCK)
	if err != nil {
		t.Skipf("no pidfd that polls here: %v", err)
	}
	unix.Close(fd)
	child := exec.Command("sleep", "0.3")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	done := make(chan time.Duration)
	go func() {
		awaitExit(child.Process.Pid)
		done <- time.Since(started)
	}()
	time.Sleep(100 * time.Millisecond)
	stacks := make([]byte, 1<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	for _, stack := range strings.Split(string(stacks), "\n\n") {
		if strings.Contains(stack, "awaitExit") && !strings.Contains(stack, "[IO wait") {
			t.Errorf("the goroutine awaiting the exit is not in the poller:\n%s", stack)
		}
	}
	if waited := <-done; waited < 250*time.Millisecond {
		t.Errorf("awaitExit returned after %v, before the child exited", waited)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
}

// The daemon names its client, and with a shared access, admits the owner
// without reading its groups.
func TestCheckDaemonPeerNamesTheClient(t *testing.T) {
	server, client := connectedUnixPair(t)
	defer client.Close()
	defer server.Close()
	peer, err := checkDaemonPeer(server, daemonAccess{shared: true, group: "proj", gid: 2147483000})
	if err != nil || peer.UID != os.Geteuid() || peer.GID != os.Getegid() {
		t.Fatalf("peer=(%+v, %v)", peer, err)
	}
}

// SO_PEERGROUPS, and on an older kernel /proc, tell the client's groups.
func TestPeerGroupsAreTheClientsGroups(t *testing.T) {
	server, client := connectedUnixPair(t)
	defer client.Close()
	defer server.Close()
	want, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := server.(*net.UnixConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var groups []int
	var groupsErr error
	if err := raw.Control(func(fd uintptr) {
		groups, groupsErr = peerGroups(int(fd), os.Getpid())
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(groups)
	slices.Sort(want)
	if groupsErr != nil || !slices.Equal(groups, want) {
		t.Fatalf("peer groups=(%v, %v), want %v", groups, groupsErr, want)
	}
	process, err := processGroups(os.Getpid())
	slices.Sort(process)
	if err != nil || !slices.Equal(process, want) {
		t.Fatalf("process groups=(%v, %v), want %v", process, err, want)
	}
}

func connectedUnixPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	client, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	if server == nil {
		t.Fatal("accepting the client failed")
	}
	return server, client
}
