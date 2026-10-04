// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmd

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A child's exit is awaited in the poller, not in a system call that holds a
// thread.
func TestAwaitExitWaitsInThePoller(t *testing.T) {
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
