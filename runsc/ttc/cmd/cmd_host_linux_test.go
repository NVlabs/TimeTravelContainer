// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmd

import (
	"os"
	"os/exec"
	"runtime"
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
